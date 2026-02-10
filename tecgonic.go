package tecgonic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
}

// New creates a new Compiler, initializing the WASM runtime and pre-compiling
// the Tectonic module. This is a one-time cost.
func New(ctx context.Context, opts ...CompilerOption) (*Compiler, error) {
	var cfg compilerConfig
	for _, o := range opts {
		o(&cfg)
	}

	rtConfig := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	rt := wazero.NewRuntimeWithConfig(ctx, rtConfig)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("tecgonic: instantiating WASI: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, wasm.TectonicWASM)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("tecgonic: compiling WASM module: %w", err)
	}

	return &Compiler{
		runtime:  rt,
		compiled: compiled,
		config:   cfg,
	}, nil
}

// Close releases the WASM runtime and all associated resources.
func (c *Compiler) Close(ctx context.Context) error {
	return c.runtime.Close(ctx)
}

// GenerateFormat generates the LaTeX format file (latex.fmt) in the bundle directory.
// This must be called once after extracting a bundle before compilations can succeed.
// If latex.fmt already exists in bundleDir, this is a no-op.
func (c *Compiler) GenerateFormat(ctx context.Context, bundleDir string) error {
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
	defer os.RemoveAll(tmpDir)

	inputDir := filepath.Join(tmpDir, "input")
	outputDir := filepath.Join(tmpDir, "output")
	cacheDir := filepath.Join(tmpDir, "cache")
	fontsDir := filepath.Join(tmpDir, "fonts")

	for _, dir := range []string{inputDir, outputDir, cacheDir, fontsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("tecgonic: creating directory %s: %w", dir, err)
		}
	}

	var stderrBuf bytes.Buffer

	fsConfig := wazero.NewFSConfig().
		WithDirMount(inputDir, "/input").
		WithDirMount(outputDir, "/output").
		WithReadOnlyDirMount(bundleDir, "/bundle").
		WithDirMount(fontsDir, "/fonts").
		WithDirMount(cacheDir, "/cache")

	modConfig := wazero.NewModuleConfig().
		WithName("").
		WithStdout(io.Discard).
		WithStderr(&stderrBuf).
		WithFSConfig(fsConfig).
		WithEnv("TECTONIC_FONT_DIR", "/fonts").
		WithEnv("TECTONIC_CACHE_DIR", "/cache")

	mod, err := c.runtime.InstantiateModule(ctx, c.compiled, modConfig)
	if err != nil {
		return fmt.Errorf("tecgonic: instantiating module for format generation: %w", err)
	}
	defer mod.Close(ctx)

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
	defer os.RemoveAll(tmpDir)

	inputDir := filepath.Join(tmpDir, "input")
	outputDir := filepath.Join(tmpDir, "output")
	cacheDir := filepath.Join(tmpDir, "cache")

	for _, dir := range []string{inputDir, outputDir, cacheDir} {
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

	// Write TeX source to input directory
	texPath := filepath.Join(inputDir, "input.tex")
	if err := os.WriteFile(texPath, texSource, 0o644); err != nil {
		return nil, fmt.Errorf("tecgonic: writing input.tex: %w", err)
	}

	// Set up stderr capture
	var stderrBuf bytes.Buffer
	var stderrWriter io.Writer = &stderrBuf
	if cfg.stderr != nil {
		stderrWriter = io.MultiWriter(&stderrBuf, cfg.stderr)
	}

	// Configure filesystem mounts
	fsConfig := wazero.NewFSConfig().
		WithDirMount(inputDir, "/input").
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
	defer mod.Close(ctx)

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
	pdfBytes, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("tecgonic: reading output PDF: %w (tectonic output: %s)", err, stderrBuf.String())
	}

	return pdfBytes, nil
}
