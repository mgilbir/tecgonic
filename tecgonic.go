package tecgonic

import (
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

	// andsifr is a fork of github.com/tetratelabs/wazero carrying compiler
	// optimizations for tecgonic's workload; the root package keeps the
	// upstream package name.
	wazero "github.com/mgilbir/andsifr"
	"github.com/mgilbir/andsifr/api"
	"github.com/mgilbir/andsifr/imports/wasi_snapshot_preview1"
	"github.com/mgilbir/tecgonic/wasm"
)

// isStateFile reports whether name is a TeX feedback file worth round-tripping
// through a state directory (see WithStateDir): a docBase.* intermediate that a
// later pass reads back to converge — .aux, .toc, .out, .bbl, beamer's
// .nav/.snm/.vrb, makeidx's .idx/.ind, glossaries, biber's .run.xml, and so on.
// docBase is the main source's basename without its .tex extension (paper.tex
// -> "paper"), since the engine names its intermediates after the jobname.
//
// Rather than an allowlist (which silently gave beamer and index documents no
// speedup, audit C20), everything the engine writes to the output directory is
// harvested except the primary outputs and the source: seeding those back
// cannot help convergence, and re-seeding the source would clobber a real input.
func isStateFile(name, docBase string) bool {
	if !strings.HasPrefix(name, docBase+".") {
		return false
	}
	switch filepath.Ext(name) {
	case ".pdf", ".log", ".xdv", ".gz", ".tex":
		// .gz catches <docBase>.synctex.gz; .tex is the source document.
		return false
	}
	return true
}

// maxStderrBytes bounds how much of the engine's stderr is retained in memory
// per compile. A hostile document can emit unbounded diagnostics; keeping the
// most recent bytes preserves the fatal-error tail (which classification and
// error messages rely on) while capping memory.
const maxStderrBytes = 1 << 20 // 1 MiB

// boundedBuffer is an io.Writer that retains at most max bytes, keeping the most
// recent ones and recording whether earlier output was dropped.
type boundedBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf = append(b.buf, p...)
	if b.max > 0 && len(b.buf) > b.max {
		b.truncated = true
		keep := make([]byte, b.max)
		copy(keep, b.buf[len(b.buf)-b.max:])
		b.buf = keep
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	if b.truncated {
		return "[earlier tectonic output truncated]\n" + string(b.buf)
	}
	return string(b.buf)
}

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
	if cfg.memoryLimitPages > 0 {
		rtConfig = rtConfig.WithMemoryLimitPages(cfg.memoryLimitPages)
	}

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

	compiled, err := rt.CompileModule(ctx, wasm.Module())
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
//
// If bundleDir is empty, the compiler's default bundle directory (set with
// WithDefaultBundleDir) is used, so the directory need not be repeated when it
// matches the compiler's default.
func (c *Compiler) GenerateFormat(ctx context.Context, bundleDir string, opts ...GenerateFormatOption) error {
	var fmtCfg generateFormatConfig
	for _, o := range opts {
		o(&fmtCfg)
	}
	if bundleDir == "" {
		bundleDir = c.config.defaultBundleDir
	}
	if bundleDir == "" {
		return fmt.Errorf("tecgonic: no bundle directory specified (pass one or use WithDefaultBundleDir)")
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

	inputDir := filepath.Join(tmpDir, "input")
	outputDir := filepath.Join(tmpDir, "output")
	cacheDir := filepath.Join(tmpDir, "cache")
	fontsDir := filepath.Join(tmpDir, "fonts")

	for _, dir := range []string{inputDir, outputDir, cacheDir, fontsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("tecgonic: creating directory %s: %w", dir, err)
		}
	}

	stderrBuf := &boundedBuffer{max: maxStderrBytes}
	var stderrWriter io.Writer = stderrBuf
	if fmtCfg.stderr != nil {
		stderrWriter = io.MultiWriter(stderrBuf, fmtCfg.stderr)
	}

	fsConfig := wazero.NewFSConfig().
		WithDirMount(inputDir, "/input").
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return newEngineError(err, 0, stderrBuf.String())
		}
		return fmt.Errorf("tecgonic: instantiating module for format generation: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	fn := mod.ExportedFunction("tectonic_generate_format")
	if fn == nil {
		return fmt.Errorf("tecgonic: exported function tectonic_generate_format not found (rebuild WASM module with updated upstream)")
	}

	results, callErr := fn.Call(ctx)
	if callErr != nil {
		return newEngineError(callErr, 0, stderrBuf.String())
	}
	if len(results) > 0 && results[0] != 0 {
		return newEngineError(nil, int32(results[0]), stderrBuf.String())
	}

	// Find the generated format file in cache and copy to bundle dir. Prefer
	// the expected latex.fmt; fall back to a lone *.fmt (a differently-named
	// build), but refuse to guess among several rather than silently rebranding
	// an arbitrary one as latex.fmt (audit C19).
	fmtPath := filepath.Join(cacheDir, "latex.fmt")
	if _, err := os.Stat(fmtPath); err != nil {
		entries, _ := os.ReadDir(cacheDir)
		var candidates []string
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".fmt" {
				candidates = append(candidates, e.Name())
			}
		}
		switch len(candidates) {
		case 0:
			return fmt.Errorf("tecgonic: no format file generated in cache (tectonic output: %s)", stderrBuf.String())
		case 1:
			fmtPath = filepath.Join(cacheDir, candidates[0])
		default:
			return fmt.Errorf("tecgonic: multiple format files generated (%v); expected latex.fmt", candidates)
		}
	}

	fmtData, err := os.ReadFile(fmtPath)
	if err != nil {
		return fmt.Errorf("tecgonic: reading generated format file: %w", err)
	}

	// Write atomically: Compile mounts bundleDir read-only and the engine loads
	// latex.fmt from it, possibly concurrently (or across processes sharing the
	// bundle). A temp-file-plus-rename ensures a reader sees either no format
	// file or a complete one, never a torn multi-megabyte write.
	if err := writeFileAtomic(filepath.Join(bundleDir, "latex.fmt"), fmtData, 0o644); err != nil {
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
// ...). For a single, self-contained source, see CompileSource.
//
// mainName is a slash-separated path into fsys (e.g. "paper.tex" or
// "src/paper.tex") and must exist in fsys. References inside the document
// resolve relative to the main source's own directory, mirroring how a TeX
// engine treats the file you hand it. The output PDF is named after the
// basename, so both "paper.tex" and "src/paper.tex" produce "paper.pdf".
//
// fsys defines the document's entire input visibility — the module reads
// exactly what fsys serves and nothing else — which also makes it a trust
// boundary the caller owns. Note that os.DirFS follows symbolic links,
// including ones pointing outside its root; use an in-memory fs.FS (or fs.Sub
// of a vetted tree) for untrusted input.
//
// Each call creates an isolated WASM instance with its own filesystem.
func (c *Compiler) Compile(ctx context.Context, fsys fs.FS, mainName string, opts ...CompileOption) ([]byte, error) {
	cfg, err := c.resolveCompileConfig(opts)
	if err != nil {
		return nil, err
	}
	if fsys == nil {
		return nil, fmt.Errorf("tecgonic: no input filesystem provided")
	}
	if info, err := fs.Stat(fsys, mainName); err != nil {
		return nil, fmt.Errorf("tecgonic: main source %q not found in input fs: %w", mainName, err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("tecgonic: main source %q is a directory, not a file", mainName)
	}

	// The engine names its intermediates and output after the main source's
	// basename (its jobname), so state files are keyed on that, not "input".
	docBase := strings.TrimSuffix(path.Base(mainName), ".tex")

	// Seed feedback state files from a previous compile so the engine can
	// converge in a single pass (see WithStateDir). The caller's input fs is
	// read-only, so seeds are served through a read-only overlay next to the
	// main source; a file already present in fsys always wins.
	if cfg.stateDir != "" {
		overlay := fstest.MapFS{}
		forEachStateFile(cfg.stateDir, docBase, func(name string, data []byte) {
			seedName := name
			if dir := path.Dir(mainName); dir != "." {
				seedName = path.Join(dir, name)
			}
			overlay[seedName] = &fstest.MapFile{Data: data, Mode: 0o644}
		})
		if len(overlay) > 0 {
			fsys = &overlayFS{base: fsys, overlay: overlay}
		}
	}

	return c.runEngine(ctx, cfg, docBase, path.Join("/input", mainName),
		func(string) (func(wazero.FSConfig) wazero.FSConfig, error) {
			return func(fc wazero.FSConfig) wazero.FSConfig {
				return fc.WithFSMount(fsys, "/input")
			}, nil
		})
}

// CompileSource compiles a single, self-contained LaTeX source to PDF, served
// to the engine as "input.tex".
//
// Unlike Compile, it stages the source in a host temp directory rather than an
// fs.FS mount: the engine's file-search probing then hits raw host syscalls
// instead of the fs.FS adapter, keeping this common single-file path as fast as
// a direct compile. For multi-file documents (\input, \includegraphics, .bib,
// .cls, .sty, ...) use Compile with an fs.FS.
func (c *Compiler) CompileSource(ctx context.Context, texSource []byte, opts ...CompileOption) ([]byte, error) {
	cfg, err := c.resolveCompileConfig(opts)
	if err != nil {
		return nil, err
	}
	const docBase = "input"

	return c.runEngine(ctx, cfg, docBase, "/input/input.tex",
		func(tmpDir string) (func(wazero.FSConfig) wazero.FSConfig, error) {
			inputDir := filepath.Join(tmpDir, "input")
			if err := os.MkdirAll(inputDir, 0o755); err != nil {
				return nil, fmt.Errorf("tecgonic: creating input dir: %w", err)
			}
			if err := os.WriteFile(filepath.Join(inputDir, "input.tex"), texSource, 0o644); err != nil {
				return nil, fmt.Errorf("tecgonic: writing input.tex: %w", err)
			}
			// Seed state files directly into the private input dir (a plain
			// write suffices; the dir is not shared).
			if cfg.stateDir != "" {
				var seedErr error
				forEachStateFile(cfg.stateDir, docBase, func(name string, data []byte) {
					if seedErr == nil {
						seedErr = os.WriteFile(filepath.Join(inputDir, name), data, 0o644)
					}
				})
				if seedErr != nil {
					return nil, fmt.Errorf("tecgonic: seeding state file: %w", seedErr)
				}
			}
			return func(fc wazero.FSConfig) wazero.FSConfig {
				return fc.WithDirMount(inputDir, "/input")
			}, nil
		})
}

// resolveCompileConfig merges the per-call options over the compiler defaults
// and validates them.
func (c *Compiler) resolveCompileConfig(opts []CompileOption) (compileConfig, error) {
	cfg := compileConfig{
		bundleDir: c.config.defaultBundleDir,
		fontsDir:  c.config.defaultFontsDir,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.err != nil {
		return cfg, cfg.err
	}
	if cfg.bundleDir == "" {
		return cfg, fmt.Errorf("tecgonic: no bundle directory specified (use WithDefaultBundleDir or WithBundleDir)")
	}
	if err := validateBundleDir(cfg.bundleDir); err != nil {
		return cfg, err
	}
	if cfg.fontsDir != "" {
		if info, err := os.Stat(cfg.fontsDir); err != nil {
			return cfg, fmt.Errorf("tecgonic: fonts directory %q: %w", cfg.fontsDir, err)
		} else if !info.IsDir() {
			return cfg, fmt.Errorf("tecgonic: fonts directory %q is not a directory", cfg.fontsDir)
		}
	}
	return cfg, nil
}

// validateBundleDir checks that dir exists and holds a generated latex.fmt, so a
// typo'd path or a bundle that never had GenerateFormat run against it fails with
// a config-shaped error at the API boundary — instead of the engine mounting a
// phantom directory (wazero does not check) and aborting mid-compile, a failure
// that would otherwise be misread as a document error (audit C6/C1).
func validateBundleDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("tecgonic: bundle directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("tecgonic: bundle directory %q is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "latex.fmt")); err != nil {
		return fmt.Errorf("tecgonic: bundle directory %q has no latex.fmt (run Compiler.GenerateFormat first): %w", dir, err)
	}
	return nil
}

// forEachStateFile invokes fn for each harvested feedback file in stateDir that
// belongs to docBase (see isStateFile), reading its contents.
func forEachStateFile(stateDir, docBase string, fn func(name string, data []byte)) {
	entries, _ := os.ReadDir(stateDir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isStateFile(name, docBase) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil {
			continue
		}
		fn(name, data)
	}
}

// runEngine performs one isolated engine run: it creates the output, cache, and
// fonts mounts, lets the caller attach the /input mount (an fs.FS or a host
// directory) via mountInput, runs tectonic_compile, harvests state files, and
// returns the PDF named after docBase.
func (c *Compiler) runEngine(ctx context.Context, cfg compileConfig, docBase, guestMainPath string, prepareInput func(tmpDir string) (func(wazero.FSConfig) wazero.FSConfig, error)) ([]byte, error) {
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

	fontsDir := cfg.fontsDir
	if fontsDir == "" {
		fontsDir = filepath.Join(tmpDir, "fonts")
		if err := os.MkdirAll(fontsDir, 0o755); err != nil {
			return nil, fmt.Errorf("tecgonic: creating fonts dir: %w", err)
		}
	}

	mountInput, err := prepareInput(tmpDir)
	if err != nil {
		return nil, err
	}

	// Capture stderr into a bounded buffer (audit C15).
	stderrBuf := &boundedBuffer{max: maxStderrBytes}
	var stderrWriter io.Writer = stderrBuf
	if cfg.stderr != nil {
		stderrWriter = io.MultiWriter(stderrBuf, cfg.stderr)
	}

	// Output and cache are host temp dirs; bundle and fonts are read-only.
	fsConfig := mountInput(wazero.NewFSConfig().
		WithDirMount(outputDir, "/output").
		WithReadOnlyDirMount(cfg.bundleDir, "/bundle").
		WithReadOnlyDirMount(fontsDir, "/fonts").
		WithDirMount(cacheDir, "/cache"))

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

	mod, err := c.runtime.InstantiateModule(ctx, c.compiled, modConfig)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, newEngineError(err, 0, stderrBuf.String())
		}
		return nil, fmt.Errorf("tecgonic: instantiating module: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	// Compile with an explicit primary input path so the entry filename is
	// caller-controlled; the engine derives the jobname from its basename.
	exitCode, callErr := callTectonicCompile(ctx, mod, guestMainPath, "/output", "/bundle")

	// Classify a trap (callErr != nil) or a non-zero exit code into an
	// *EngineError: a cancellation, a TeX error, or an engine fault (audit
	// C2/C8).
	if callErr != nil {
		return nil, newEngineError(callErr, 0, stderrBuf.String())
	}
	if exitCode != 0 {
		return nil, newEngineError(nil, exitCode, stderrBuf.String())
	}

	// Harvest feedback state files for the next compile (see WithStateDir).
	// Documents that produce none are skipped; failures are not fatal. Writes
	// are atomic so a concurrent compile seeding from this directory never reads
	// a half-written file (audit C7).
	if cfg.stateDir != "" {
		if err := os.MkdirAll(cfg.stateDir, 0o755); err == nil {
			entries, _ := os.ReadDir(outputDir)
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !isStateFile(name, docBase) {
					continue
				}
				data, err := os.ReadFile(filepath.Join(outputDir, name))
				if err != nil {
					continue
				}
				_ = writeFileAtomic(filepath.Join(cfg.stateDir, name), data, 0o644)
			}
		}
	}

	// Read the output PDF, named after the main source's basename.
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

// overlayFS serves seeded state files (see WithStateDir) alongside the caller's
// input filesystem. The base filesystem always wins: overlay entries are only
// consulted when the base reports the file as absent, so a caller who ships
// their own .aux keeps full control.
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
