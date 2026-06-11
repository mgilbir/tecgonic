package tecgonic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing/fstest"

	"github.com/mgilbir/tecgonic/wasm"
	"github.com/tetratelabs/wazero"
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

// Compile compiles the given LaTeX source to PDF.
// Each call creates an isolated WASM instance with its own filesystem.
func (c *Compiler) Compile(ctx context.Context, texSource []byte, opts ...CompileOption) ([]byte, error) {
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

	// Assemble the main source and any auxiliary inputs into an in-memory
	// filesystem; the WASM module never sees host paths for its inputs.
	inputFS, err := buildInputFS(texSource, cfg.inputFiles, cfg.inputFS)
	if err != nil {
		return nil, err
	}

	// Set up stderr capture
	var stderrBuf bytes.Buffer
	var stderrWriter io.Writer = &stderrBuf
	if cfg.stderr != nil {
		stderrWriter = io.MultiWriter(&stderrBuf, cfg.stderr)
	}

	// Configure filesystem mounts
	fsConfig := wazero.NewFSConfig().
		WithFSMount(inputFS, "/input").
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

	// Call tectonic_compile_defaults
	fn := mod.ExportedFunction("tectonic_compile_defaults")
	if fn == nil {
		return nil, fmt.Errorf("tecgonic: exported function tectonic_compile_defaults not found")
	}

	results, callErr := fn.Call(ctx)

	// Handle WASM trap (callErr != nil)
	if callErr != nil {
		return nil, &CompileError{
			ExitCode: 2,
			Logs:     stderrBuf.String(),
			WasmErr:  callErr,
		}
	}

	// Handle non-zero exit code
	if len(results) > 0 && results[0] != 0 {
		return nil, &CompileError{
			ExitCode: int32(results[0]),
			Logs:     stderrBuf.String(),
		}
	}

	// Read the output PDF
	pdfPath := filepath.Join(outputDir, "input.pdf")

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

// mainInputName is the filename of the main LaTeX source written into the
// compilation input root. It is reserved: auxiliary input files may not use it.
const mainInputName = "input.tex"

// buildInputFS assembles the in-memory filesystem mounted read-only at /input
// in the WASM module. It always contains the main source as input.tex, plus
// any auxiliary files from WithInputFiles and a snapshot of any fs.FS from
// WithInputFS. Inputs never touch the host filesystem.
//
// Map keys are treated as logical, slash-separated paths (matching LaTeX's
// \input convention) and validated independently of the host OS: they must be
// relative, must not escape the input root via "..", and must not collide with
// the main source. Paths from the fs.FS are valid by construction (fs.ValidPath)
// but are checked against the same collisions.
func buildInputFS(texSource []byte, files map[string][]byte, fsys fs.FS) (fs.FS, error) {
	m := fstest.MapFS{
		mainInputName: &fstest.MapFile{Data: texSource, Mode: 0o644},
	}

	for name, data := range files {
		clean, err := validateInputPath(name)
		if err != nil {
			return nil, err
		}
		m[clean] = &fstest.MapFile{Data: data, Mode: 0o644}
	}

	if fsys != nil {
		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("tecgonic: walking input fs at %q: %w", p, err)
			}
			if d.IsDir() {
				return nil
			}
			if p == mainInputName {
				return fmt.Errorf("tecgonic: input fs path %q is reserved for the main source", p)
			}
			if _, exists := m[p]; exists {
				return fmt.Errorf("tecgonic: input fs path %q collides with an input file", p)
			}
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return fmt.Errorf("tecgonic: reading input fs file %q: %w", p, err)
			}
			m[p] = &fstest.MapFile{Data: data, Mode: 0o644}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return m, nil
}

// validateInputPath normalizes a WithInputFiles key to its slash-separated
// form and checks that the result is something the in-memory filesystem can
// actually serve. Since inputs never touch the host filesystem, this is not a
// security boundary: it exists to fail fast on keys that fs.FS semantics make
// unreachable (absolute paths, escapes via "..", empty names) and on the
// reserved main-source name, which would otherwise silently overwrite the
// main document.
func validateInputPath(name string) (string, error) {
	clean := path.Clean(filepath.ToSlash(name))
	if clean == "." || !fs.ValidPath(clean) {
		return "", fmt.Errorf("tecgonic: invalid input file path %q", name)
	}
	if clean == mainInputName {
		return "", fmt.Errorf("tecgonic: input file path %q is reserved for the main source", name)
	}
	return clean, nil
}
