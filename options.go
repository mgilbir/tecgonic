package tecgonic

import "io"

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
