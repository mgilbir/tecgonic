package tecgonic

import (
	"fmt"
	"io"
	"time"
)

// compilerConfig holds configuration set once on New().
type compilerConfig struct {
	defaultBundleDir    string
	defaultFontsDir     string
	compilationCacheDir string
	contextCancellation bool
	memoryLimitPages    uint32 // 0 = wazero default (65536 pages = 4 GiB)
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

// maxWasmPages is the wasm32 linear-memory ceiling: 65536 pages * 64 KiB = 4 GiB.
const maxWasmPages = 65536

// WithMemoryLimitMiB caps the WebAssembly linear memory each compilation may
// allocate. Without it, a single compile can grow to the wasm32 ceiling of
// 4 GiB, so N concurrent compiles of hostile or pathological documents can
// exhaust host memory. wazero rounds the limit up to whole 64 KiB pages.
//
// The effective range is 1..4096 MiB. Values below 1 are ignored (no limit).
// A value above 4096 is clamped to 4096 (the wasm32 ceiling): a larger cap
// cannot constrain a 32-bit address space anyway, so clamping delivers exactly
// what such a request means — "effectively no limit" — instead of panicking, as
// an unclamped page count above 65536 would (audit C4).
//
// This bounds only WASM memory. A compilation also writes to on-disk temp
// directories (its output and cache mounts), which this does not limit; bound
// untrusted input's disk use at the filesystem or container level.
func WithMemoryLimitMiB(mib int) CompilerOption {
	return func(c *compilerConfig) {
		if mib < 1 {
			return
		}
		pages := int64(mib) * 16 // 1 MiB = 16 * 64 KiB pages; int64 avoids overflow
		if pages > maxWasmPages {
			pages = maxWasmPages
		}
		c.memoryLimitPages = uint32(pages)
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
	bundleDir    string
	fontsDir     string
	stderr       io.Writer
	output       io.Writer
	maxPasses    int
	stateDir     string
	buildDate    time.Time // date for \today and the PDF timestamp
	buildDateSet bool      // whether WithBuildDate was given (else default to now)
	err          error     // first invalid-option error, surfaced by Compile
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
//
// n must be at least 1; Compile returns an error for a smaller value rather
// than silently falling back to the default, so a mis-wired 0 (e.g. an unset
// config field) is not mistaken for "cap at zero passes".
func WithMaxPasses(n int) CompileOption {
	return func(c *compileConfig) {
		if n < 1 {
			if c.err == nil {
				c.err = fmt.Errorf("tecgonic: WithMaxPasses(%d): must be at least 1", n)
			}
			return
		}
		c.maxPasses = n
	}
}

// WithStateDir persists TeX feedback files (.aux, .toc, .out, .bbl, and the
// analogous intermediates for beamer, indexes, glossaries, and biber) in dir
// across Compile calls. Use one directory per logical document.
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
// first use and updated (atomically) after each successful compile. Do not
// share one directory between Compile calls that run concurrently: even for the
// same document, one call may seed from the directory while another is writing
// it.
func WithStateDir(dir string) CompileOption {
	return func(c *compileConfig) {
		c.stateDir = dir
	}
}

// WithBuildDate sets the date the compilation sees as "now": it drives \today,
// \year/\month/\day, and the PDF's timestamp. Without it, the current host date
// (at the moment of the call) is used.
//
// The WebAssembly sandbox has no real clock, so the date is passed to the engine
// as a single fixed value (via SOURCE_DATE_EPOCH); the document observes a
// constant, never a running clock, so this exposes no wall-clock timing
// side-channel and keeps compilation deterministic within a call. Pin a fixed
// date here for reproducible output — two compiles of the same document with the
// same WithBuildDate produce byte-identical PDFs.
//
// A time before the Unix epoch (1970) is clamped to the epoch. This requires a
// WASM module built with SOURCE_DATE_EPOCH support (ABI 2); New rejects an older
// module, so the option cannot silently no-op.
func WithBuildDate(t time.Time) CompileOption {
	return func(c *compileConfig) {
		c.buildDate = t
		c.buildDateSet = true
	}
}
