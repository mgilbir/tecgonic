package tecgonic

import "io"

// compilerConfig holds configuration set once on New().
type compilerConfig struct {
	defaultBundleDir    string
	defaultFontsDir     string
	compilationCacheDir string
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
