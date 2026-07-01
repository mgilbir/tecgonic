package tecgonic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing/fstest"

	"github.com/mgilbir/tecgonic/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// stateFileExts are the extensions of the TeX feedback files round-tripped by
// WithStateDir. The WASM module exports them (named after the main source's
// basename) to the output directory after a successful compile; Compile
// overlays them onto the input filesystem on the next run.
var stateFileExts = []string{".aux", ".toc", ".lof", ".lot", ".out", ".bbl"}

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

	// WithCloseOnContextDone makes running compilations interruptible via the
	// context, but it forces wazero to insert termination checks on every loop
	// and call, which is ~5x slower on CPU-heavy documents. It is therefore
	// opt-in via WithContextCancellation().
	rtConfig := wazero.NewRuntimeConfig().WithCloseOnContextDone(cfg.contextCancellation)

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

	// Format generation reads only the bundle and writes to the cache; it never
	// touches /input, so no input filesystem is mounted here.
	fsConfig := wazero.NewFSConfig().
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
// references. fsys is passed to the WASM module as-is and is never written to
// the host filesystem; any fs.FS works (an embed.FS, os.DirFS, fstest.MapFS,
// ...).
//
// mainName is a slash-separated path into fsys (e.g. "paper.tex" or
// "src/paper.tex") and must exist in fsys; validity of the path is the
// responsibility of the fsys provider. The output PDF is named after its
// basename, so both "paper.tex" and "src/paper.tex" produce "paper.pdf".
//
// References inside the document resolve relative to the main source's own
// directory, mirroring how a TeX engine treats the file you hand it. If the
// main source is at the fsys root (e.g. "paper.tex"), \input{sections/intro}
// reads "sections/intro.tex". If the main source is "src/paper.tex", the same
// \input{sections/intro} reads "src/sections/intro.tex" — not "sections/intro"
// at the root. Reach files in ancestor directories with relative paths such as
// \input{../shared/macros}.
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

	// Seed feedback state files from a previous compile of this document so
	// the engine can converge in a single pass (see WithStateDir). The input
	// filesystem is read-only and caller-owned, so the state files are served
	// through an overlay next to the main source rather than written anywhere;
	// files present in the caller's filesystem always win.
	docBase := strings.TrimSuffix(path.Base(mainName), ".tex")
	if cfg.stateDir != "" {
		overlay := fstest.MapFS{}
		for _, ext := range stateFileExts {
			data, err := os.ReadFile(filepath.Join(cfg.stateDir, docBase+ext))
			if err != nil {
				continue
			}
			name := docBase + ext
			if dir := path.Dir(mainName); dir != "." {
				name = path.Join(dir, name)
			}
			overlay[name] = &fstest.MapFile{Data: data, Mode: 0o644}
		}
		if len(overlay) > 0 {
			fsys = &overlayFS{base: fsys, overlay: overlay}
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

	if cfg.maxPasses > 0 {
		modConfig = modConfig.WithEnv("TECTONIC_MAX_PASSES", strconv.Itoa(cfg.maxPasses))
	}

	// Instantiate a fresh module for this compilation
	mod, err := c.runtime.InstantiateModule(ctx, c.compiled, modConfig)
	if err != nil {
		return nil, fmt.Errorf("tecgonic: instantiating module: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	// Compile with an explicit primary input path so the entry filename is
	// caller-controlled rather than hardcoded in the WASM module. The path is
	// made absolute under the /input mount so the engine splits it into a
	// directory and a basename: the output PDF is named after the basename
	// (paper.tex -> paper.pdf) and relative \input/\includegraphics references
	// resolve against the main source's own directory. A bare relative path
	// would fold any subdirectory into the output base name and never match the
	// engine's jobname.
	exitCode, callErr := callTectonicCompile(ctx, mod, path.Join("/input", mainName), "/output", "/bundle")

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

	// Harvest feedback state files for the next compile (see WithStateDir).
	// The WASM module exports them flat under /output, named after the main
	// source's basename. Missing files (documents that produce none) are
	// skipped; state persistence failures are not fatal to the compilation.
	if cfg.stateDir != "" {
		if err := os.MkdirAll(cfg.stateDir, 0o755); err == nil {
			for _, ext := range stateFileExts {
				data, err := os.ReadFile(filepath.Join(outputDir, docBase+ext))
				if err != nil {
					continue
				}
				_ = os.WriteFile(filepath.Join(cfg.stateDir, docBase+ext), data, 0o644)
			}
		}
	}

	// Read the output PDF. The WASM module derives the output base name from
	// the primary input's basename, mirroring tectonic's jobname behaviour, and
	// writes it flat under /output regardless of any subdirectory in mainName.
	pdfPath := filepath.Join(outputDir, docBase+".pdf")

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

// CompileSource compiles a single, self-contained LaTeX source to PDF. It is a
// convenience wrapper around Compile for documents with no auxiliary inputs:
// the source is served to the WASM module as "input.tex".
//
// For multi-file documents (\input, \includegraphics, .bib, .cls, .sty, ...),
// use Compile with an fs.FS.
func (c *Compiler) CompileSource(ctx context.Context, texSource []byte, opts ...CompileOption) ([]byte, error) {
	const mainName = "input.tex"
	fsys := fstest.MapFS{
		mainName: &fstest.MapFile{Data: texSource, Mode: 0o644},
	}
	return c.Compile(ctx, fsys, mainName, opts...)
}

// overlayFS serves seeded state files (see WithStateDir) alongside the
// caller's input filesystem. The base filesystem always wins: overlay entries
// are only consulted when the base reports the file as absent, so a caller
// who ships their own .aux keeps full control.
type overlayFS struct {
	base    fs.FS
	overlay fstest.MapFS
}

func (o *overlayFS) Open(name string) (fs.File, error) {
	f, err := o.base.Open(name)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	if _, ok := o.overlay[name]; ok {
		return o.overlay.Open(name)
	}
	return nil, err
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
