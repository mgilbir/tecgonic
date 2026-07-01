package tecgonic

import (
	"io"
)

// compilerConfig holds configuration set once on New().
type compilerConfig struct {
	defaultBundleDir    string
	defaultFontsDir     string
	compilationCacheDir string
	contextCancellation bool
}

// CompilerOption configures a Compiler at creation time.
type CompilerOption func(*compilerConfig)

// WithDefaultBundleDir sets the default bundle directory for all compilations.
func WithDefaultBundleDir(dir string) CompilerOption {
	return func(c *compilerConfig) {
		c.defaultBundleDir = dir
	}
}

// WithDefaultFontsDir sets the default fonts directory for all compilations.
func WithDefaultFontsDir(dir string) CompilerOption {
	return func(c *compilerConfig) {
		c.defaultFontsDir = dir
	}
}

// WithCompilationCache enables caching of the compiled WASM module on disk.
// Subsequent New() calls with the same directory will skip WASM compilation.
func WithCompilationCache(dir string) CompilerOption {
	return func(c *compilerConfig) {
		c.compilationCacheDir = dir
	}
}

// WithContextCancellation makes Compile honor context cancellation and
// deadlines while a compilation is running.
//
// It is OFF by default for performance: enabling it forces wazero to insert a
// termination check on every loop back-edge and function call, which slows
// CPU-heavy documents dramatically. Measured on a large tabularray longtblr
// table, enabling it was ~5x slower (34s vs 164s).
//
// When this option is OFF (the default), wazero does not consult the context
// during a compilation at all: a Compile call runs to completion regardless of
// cancellation or deadline, and the goroutine keeps using a CPU core until it
// finishes. Enable this only when you need to abort or time out long-running
// compilations and are willing to pay the per-iteration cost.
func WithContextCancellation() CompilerOption {
	return func(c *compilerConfig) {
		c.contextCancellation = true
	}
}

// generateFormatConfig holds per-call configuration for GenerateFormat().
type generateFormatConfig struct {
	stderr io.Writer
}

// GenerateFormatOption configures a single GenerateFormat() call.
type GenerateFormatOption func(*generateFormatConfig)

// WithGenerateFormatStderr tees tectonic's diagnostic output to the given writer
// during format generation.
func WithGenerateFormatStderr(w io.Writer) GenerateFormatOption {
	return func(c *generateFormatConfig) {
		c.stderr = w
	}
}

// compileConfig holds per-call configuration for Compile().
type compileConfig struct {
	bundleDir string
	fontsDir  string
	stderr    io.Writer
	output    io.Writer
	maxPasses int
	stateDir  string
}

// CompileOption configures a single Compile() call.
type CompileOption func(*compileConfig)

// WithBundleDir overrides the bundle directory for this compilation.
func WithBundleDir(dir string) CompileOption {
	return func(c *compileConfig) {
		c.bundleDir = dir
	}
}

// WithFontsDir overrides the fonts directory for this compilation.
func WithFontsDir(dir string) CompileOption {
	return func(c *compileConfig) {
		c.fontsDir = dir
	}
}

// WithStderr tees tectonic's diagnostic output to the given writer.
func WithStderr(w io.Writer) CompileOption {
	return func(c *compileConfig) {
		c.stderr = w
	}
}

// WithOutput streams the compiled PDF to the given writer instead of returning
// it as a byte slice. When set, Compile returns (nil, nil) on success.
func WithOutput(w io.Writer) CompileOption {
	return func(c *compileConfig) {
		c.output = w
	}
}

// WithMaxPasses caps the number of TeX passes for this compilation (minimum 1).
//
// By default tectonic reruns TeX until the document's cross-reference data
// (.aux) converges, up to 6 passes; each pass repeats the full cost of the
// document. WithMaxPasses(1) skips rerun detection entirely, roughly halving
// compilation time for documents that do not use cross-references, citations,
// or tables of contents. Documents that do need extra passes may produce
// stale or missing references when capped too low.
//
// Requires a WASM module built with TECTONIC_MAX_PASSES support; older
// modules ignore this option.
func WithMaxPasses(n int) CompileOption {
	return func(c *compileConfig) {
		c.maxPasses = n
	}
}

// WithStateDir persists TeX feedback files (.aux, .toc, .lof, .lot, .out,
// .bbl) in dir across Compile calls. Use one directory per logical document.
//
// TeX resolves cross-references, tables of contents, and page totals by
// feeding data recorded during one pass into the next, so a cold compile
// always needs at least 2 passes. With a state directory, the previous
// compilation's feedback files seed the first pass; when the document's
// feedback is unchanged, the compilation converges after a single pass —
// roughly half the cost, with output identical to a full multi-pass run.
//
// This is always correct: a stale seed (e.g. after editing the document) only
// causes the usual reruns, never wrong output. The directory is created on
// first use and updated after each successful compile. Do not share one
// directory between concurrent Compile calls of different documents.
//
// The state files are named after the main source's basename (paper.tex ->
// paper.aux) and are served to the engine through a read-only overlay next to
// the main source; the caller's input filesystem is never written to, and a
// file that already exists in it always takes precedence over the seed.
func WithStateDir(dir string) CompileOption {
	return func(c *compileConfig) {
		c.stateDir = dir
	}
}
