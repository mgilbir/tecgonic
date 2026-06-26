package tecgonic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"

	"github.com/mgilbir/tecgonic/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Compiler compiles LaTeX documents to PDF using the Tectonic engine via WASM.
// It is safe for concurrent use; each Compile call gets its own WASM instance.
type Compiler struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	config   compilerConfig
	cache    wazero.CompilationCache
}

// New creates a new Compiler, initializing the WASM runtime and pre-compiling
// the Tectonic module. This is a one-time cost.
func New(ctx context.Context, opts ...CompilerOption) (*Compiler, error) {
	var cfg compilerConfig
	for _, o := range opts {
		o(&cfg)
	}

	rtConfig := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)

	var cache wazero.CompilationCache
	if cfg.compilationCacheDir != "" {
		var err error
		cache, err = wazero.NewCompilationCacheWithDir(cfg.compilationCacheDir)
		if err != nil {
			return nil, fmt.Errorf("tecgonic: creating compilation cache: %w", err)
		}
		rtConfig = rtConfig.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, rtConfig)

	closeCache := func() {
		if cache != nil {
			_ = cache.Close(ctx)
		}
	}

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		closeCache()
		return nil, fmt.Errorf("tecgonic: instantiating WASI: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, wasm.TectonicWASM)
	if err != nil {
		_ = rt.Close(ctx)
		closeCache()
		return nil, fmt.Errorf("tecgonic: compiling WASM module: %w", err)
	}

	return &Compiler{
		runtime:  rt,
		compiled: compiled,
		config:   cfg,
		cache:    cache,
	}, nil
}

// Close releases the WASM runtime and all associated resources.
func (c *Compiler) Close(ctx context.Context) error {
	err := c.runtime.Close(ctx)
	if c.cache != nil {
		if cacheErr := c.cache.Close(ctx); err == nil {
			err = cacheErr
		}
	}
	return err
}

// GenerateFormat generates the LaTeX format file (latex.fmt) in the bundle directory.
// This must be called once after extracting a bundle before compilations can succeed.
// If latex.fmt already exists in bundleDir, this is a no-op.
func (c *Compiler) GenerateFormat(ctx context.Context, bundleDir string, opts ...GenerateFormatOption) error {
	var fmtCfg generateFormatConfig
	for _, o := range opts {
		o(&fmtCfg)
	}
	if bundleDir == "" {
		return fmt.Errorf("tecgonic: no bundle directory specified")
	}

	// Skip if format file already exists
	if _, err := os.Stat(filepath.Join(bundleDir, "latex.fmt")); err == nil {
		return nil
	}

	tmpDir, err := os.MkdirTemp("", "tecgonic-fmt-*")
	if err != nil {
		return fmt.Errorf("tecgonic: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	outputDir := filepath.Join(tmpDir, "output")
	cacheDir := filepath.Join(tmpDir, "cache")
	fontsDir := filepath.Join(tmpDir, "fonts")

	for _, dir := range []string{outputDir, cacheDir, fontsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("tecgonic: creating directory %s: %w", dir, err)
		}
	}

	var stderrBuf bytes.Buffer
	var stderrWriter io.Writer = &stderrBuf
	if fmtCfg.stderr != nil {
		stderrWriter = io.MultiWriter(&stderrBuf, fmtCfg.stderr)
	}

	fsConfig := wazero.NewFSConfig().
		WithFSMount(fstest.MapFS{}, "/input").
		WithDirMount(outputDir, "/output").
		WithReadOnlyDirMount(bundleDir, "/bundle").
		WithDirMount(fontsDir, "/fonts").
		WithDirMount(cacheDir, "/cache")

	modConfig := wazero.NewModuleConfig().
		WithName("").
		WithStdout(io.Discard).
		WithStderr(stderrWriter).
		WithFSConfig(fsConfig).
		WithEnv("TECTONIC_FONT_DIR", "/fonts").
		WithEnv("TECTONIC_CACHE_DIR", "/cache")

	mod, err := c.runtime.InstantiateModule(ctx, c.compiled, modConfig)
	if err != nil {
		return fmt.Errorf("tecgonic: instantiating module for format generation: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	fn := mod.ExportedFunction("tectonic_generate_format")
	if fn == nil {
		return fmt.Errorf("tecgonic: exported function tectonic_generate_format not found (rebuild WASM module with updated upstream)")
	}

	results, callErr := fn.Call(ctx)
	if callErr != nil {
		return &CompileError{
			ExitCode: 2,
			Logs:     stderrBuf.String(),
			WasmErr:  callErr,
		}
	}
	if len(results) > 0 && results[0] != 0 {
		return &CompileError{
			ExitCode: int32(results[0]),
			Logs:     stderrBuf.String(),
		}
	}

	// Find the generated format file in cache and copy to bundle dir
	fmtPath := filepath.Join(cacheDir, "latex.fmt")
	if _, err := os.Stat(fmtPath); err != nil {
		// Search for any .fmt file
		entries, _ := os.ReadDir(cacheDir)
		found := false
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".fmt" {
				fmtPath = filepath.Join(cacheDir, e.Name())
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("tecgonic: no format file generated in cache (tectonic output: %s)", stderrBuf.String())
		}
	}

	fmtData, err := os.ReadFile(fmtPath)
	if err != nil {
		return fmt.Errorf("tecgonic: reading generated format file: %w", err)
	}

	if err := os.WriteFile(filepath.Join(bundleDir, "latex.fmt"), fmtData, 0o644); err != nil {
		return fmt.Errorf("tecgonic: writing format file to bundle dir: %w", err)
	}

	return nil
}

// Compile compiles the LaTeX document rooted at mainName within fsys to PDF.
//
// fsys is served to the WASM module as the compilation input root: mainName is
// the primary source (e.g. "paper.tex") and every other file in fsys is
// available to \input, \include, \includegraphics, \bibliography, and similar
// references using the same slash-separated paths. fsys is passed to the WASM
// module as-is and is never written to the host filesystem; any fs.FS works
// (an embed.FS, os.DirFS, fstest.MapFS, ...).
//
// mainName must be a plain filename at the root of fsys (no directory
// component) and must exist in fsys. The output PDF is named after it, so
// "paper.tex" produces "paper.pdf".
//
// For a single, self-contained source with no auxiliary files, see
// CompileSource.
//
// Each call creates an isolated WASM instance with its own filesystem.
func (c *Compiler) Compile(ctx context.Context, fsys fs.FS, mainName string, opts ...CompileOption) ([]byte, error) {
	cfg := compileConfig{
		bundleDir: c.config.defaultBundleDir,
		fontsDir:  c.config.defaultFontsDir,
	}
	for _, o := range opts {
		o(&cfg)
	}

	if cfg.bundleDir == "" {
		return nil, fmt.Errorf("tecgonic: no bundle directory specified (use WithDefaultBundleDir or WithBundleDir)")
	}
	if fsys == nil {
		return nil, fmt.Errorf("tecgonic: no input filesystem provided")
	}
	if err := validateMainName(mainName); err != nil {
		return nil, err
	}
	if info, err := fs.Stat(fsys, mainName); err != nil {
		return nil, fmt.Errorf("tecgonic: main source %q not found in input fs: %w", mainName, err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("tecgonic: main source %q is a directory, not a file", mainName)
	}

	// Create isolated temp directories for this compilation
	tmpDir, err := os.MkdirTemp("", "tecgonic-*")
	if err != nil {
		return nil, fmt.Errorf("tecgonic: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	outputDir := filepath.Join(tmpDir, "output")
	cacheDir := filepath.Join(tmpDir, "cache")

	for _, dir := range []string{outputDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("tecgonic: creating directory %s: %w", dir, err)
		}
	}

	// If no fonts dir specified, create an empty one
	fontsDir := cfg.fontsDir
	if fontsDir == "" {
		fontsDir = filepath.Join(tmpDir, "fonts")
		if err := os.MkdirAll(fontsDir, 0o755); err != nil {
			return nil, fmt.Errorf("tecgonic: creating fonts dir: %w", err)
		}
	}

	// Set up stderr capture
	var stderrBuf bytes.Buffer
	var stderrWriter io.Writer = &stderrBuf
	if cfg.stderr != nil {
		stderrWriter = io.MultiWriter(&stderrBuf, cfg.stderr)
	}

	// Configure filesystem mounts. The caller's fs.FS is mounted directly as
	// the input root; the WASM module reads it lazily and never touches the
	// host filesystem for inputs.
	fsConfig := wazero.NewFSConfig().
		WithFSMount(fsys, "/input").
		WithDirMount(outputDir, "/output").
		WithReadOnlyDirMount(cfg.bundleDir, "/bundle").
		WithDirMount(fontsDir, "/fonts").
		WithDirMount(cacheDir, "/cache")

	modConfig := wazero.NewModuleConfig().
		WithName("").
		WithStdout(io.Discard).
		WithStderr(stderrWriter).
		WithFSConfig(fsConfig).
		WithEnv("TECTONIC_FONT_DIR", "/fonts").
		WithEnv("TECTONIC_CACHE_DIR", "/cache")

	// Instantiate a fresh module for this compilation
	mod, err := c.runtime.InstantiateModule(ctx, c.compiled, modConfig)
	if err != nil {
		return nil, fmt.Errorf("tecgonic: instantiating module: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	// Compile with an explicit primary input path so the entry filename is
	// caller-controlled rather than hardcoded in the WASM module.
	exitCode, callErr := callTectonicCompile(ctx, mod, mainName, "/output", "/bundle")

	// Handle WASM trap (callErr != nil)
	if callErr != nil {
		return nil, &CompileError{
			ExitCode: 2,
			Logs:     stderrBuf.String(),
			WasmErr:  callErr,
		}
	}

	// Handle non-zero exit code
	if exitCode != 0 {
		return nil, &CompileError{
			ExitCode: exitCode,
			Logs:     stderrBuf.String(),
		}
	}

	// Read the output PDF. The WASM module derives the output base name from
	// the primary input's basename, mirroring tectonic's jobname behaviour.
	pdfPath := filepath.Join(outputDir, strings.TrimSuffix(mainName, ".tex")+".pdf")

	if cfg.output != nil {
		f, err := os.Open(pdfPath)
		if err != nil {
			return nil, fmt.Errorf("tecgonic: opening output PDF: %w (tectonic output: %s)", err, stderrBuf.String())
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(cfg.output, f); err != nil {
			return nil, fmt.Errorf("tecgonic: writing PDF to output: %w", err)
		}
		return nil, nil
	}

	pdfBytes, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("tecgonic: reading output PDF: %w (tectonic output: %s)", err, stderrBuf.String())
	}

	return pdfBytes, nil
}

// defaultMainName is the filename under which CompileSource serves its source
// to the WASM module.
const defaultMainName = "input.tex"

// CompileSource compiles a single, self-contained LaTeX source to PDF. It is a
// convenience wrapper around Compile for documents with no auxiliary inputs:
// the source is served to the WASM module as "input.tex".
//
// For multi-file documents (\input, \includegraphics, .bib, .cls, .sty, ...),
// use Compile with an fs.FS.
func (c *Compiler) CompileSource(ctx context.Context, texSource []byte, opts ...CompileOption) ([]byte, error) {
	fsys := fstest.MapFS{
		defaultMainName: &fstest.MapFile{Data: texSource, Mode: 0o644},
	}
	return c.Compile(ctx, fsys, defaultMainName, opts...)
}

// validateMainName checks that name can serve as the primary source: a plain
// filename at the root of the input fs. A directory component is rejected
// because the WASM module writes the result as <basename>.pdf directly under
// /output without creating intermediate directories there.
func validateMainName(name string) error {
	if name == "" {
		return fmt.Errorf("tecgonic: empty main source name")
	}
	if !fs.ValidPath(name) || name == "." || strings.ContainsRune(name, '/') {
		return fmt.Errorf("tecgonic: main source name %q must be a plain filename at the input root", name)
	}
	return nil
}

// callTectonicCompile invokes the WASM tectonic_compile export. Its arguments
// are (pointer, length) pairs into the module's linear memory, so each string
// is allocated with the module's malloc, written into memory, and freed after
// the call. It returns the engine's exit code (0 on success).
func callTectonicCompile(ctx context.Context, mod api.Module, inputPath, outputDir, bundleDir string) (int32, error) {
	fn := mod.ExportedFunction("tectonic_compile")
	malloc := mod.ExportedFunction("malloc")
	free := mod.ExportedFunction("free")
	if fn == nil || malloc == nil || free == nil {
		return 0, fmt.Errorf("tecgonic: WASM module is missing tectonic_compile/malloc/free exports")
	}

	args := make([]uint64, 0, 6)
	var ptrs []uint64
	defer func() {
		for _, p := range ptrs {
			_, _ = free.Call(ctx, p)
		}
	}()

	for _, s := range []string{inputPath, outputDir, bundleDir} {
		b := []byte(s)
		res, err := malloc.Call(ctx, uint64(len(b)))
		if err != nil {
			return 0, fmt.Errorf("tecgonic: allocating guest memory: %w", err)
		}
		ptr := res[0]
		ptrs = append(ptrs, ptr)
		if !mod.Memory().Write(uint32(ptr), b) {
			return 0, fmt.Errorf("tecgonic: writing %d bytes to guest memory at offset %d is out of range", len(b), ptr)
		}
		args = append(args, ptr, uint64(len(b)))
	}

	results, err := fn.Call(ctx, args...)
	if err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, nil
	}
	return int32(uint32(results[0])), nil
}
