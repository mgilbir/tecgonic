package tecgonic

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fullBundleDir returns a full extracted bundle for the heavy benchmarks, which
// use packages (xcolor, tabularray) that the committed minibundle lacks. It
// skips the benchmark unless TECGONIC_BUNDLE_DIR points at a full bundle, so
// `go test -bench .` on a fresh clone skips cleanly instead of dying mid-run
// with a LaTeX missing-file error (audit C9). BenchmarkCompileSimple keeps the
// minibundle-safe bundleDir helper.
func fullBundleDir(tb testing.TB) string {
	tb.Helper()
	dir := os.Getenv("TECGONIC_BUNDLE_DIR")
	if dir == "" {
		tb.Skip("set TECGONIC_BUNDLE_DIR to a full extracted bundle to run this benchmark")
	}
	return dir
}

// longtblrDoc builds a tabularray longtblr table with the given number of body
// rows. longtblr is implemented in expl3 and does a large amount of
// macro-expansion work per row, which makes it a good stress test for the WASM
// runtime's per-iteration overhead.
func longtblrDoc(rows int) []byte {
	var b strings.Builder
	b.WriteString(`\documentclass{article}
\usepackage{xcolor}
\usepackage{tabularray}
\UseTblrLibrary{varwidth}
\begin{document}
\begin{longtblr}[caption={Bench},label={tab:bench}]{colspec={lX[2]rr},rowhead=1,hlines,vlines,row{1}={bg=gray!20,font=\bfseries}}
ID & Description & Amount & Balance \\
`)
	for i := 1; i <= rows; i++ {
		fmt.Fprintf(&b,
			"R%03d & Item number %d with some descriptive text that wraps & %d.%02d & %d.%02d \\\\\n",
			i, i, i*7, i%100, i*13, (i*3)%100)
	}
	b.WriteString("\\end{longtblr}\n\\end{document}\n")
	return []byte(b.String())
}

// BenchmarkCompileLongtblr measures compilation of a CPU-heavy longtblr table
// with and without WithContextCancellation, demonstrating the cost of wazero's
// context-termination checks. The row count defaults to 40 and can be raised
// via TECGONIC_BENCH_ROWS. Requires TECGONIC_BUNDLE_DIR (with latex.fmt).
func BenchmarkCompileLongtblr(b *testing.B) {
	dir := fullBundleDir(b)
	ctx := context.Background()

	rows := 40
	if v := os.Getenv("TECGONIC_BENCH_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rows = n
		}
	}
	doc := longtblrDoc(rows)

	run := func(b *testing.B, opts ...CompilerOption) {
		c, err := New(ctx, append([]CompilerOption{WithDefaultBundleDir(dir)}, opts...)...)
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		defer func() { _ = c.Close(ctx) }()

		b.ResetTimer()
		for b.Loop() {
			if _, err := c.CompileSource(ctx, doc); err != nil {
				b.Fatalf("Compile: %v", err)
			}
		}
	}

	// Default (fast) path: no per-iteration termination checks.
	b.Run("cancellation_off", func(b *testing.B) { run(b) })
	// Opt-in: interruptible, but wazero inserts termination checks throughout
	// the compiled code, which is dramatically slower on CPU-heavy documents.
	b.Run("cancellation_on", func(b *testing.B) { run(b, WithContextCancellation()) })
}

// BenchmarkCompileSinglePass measures the same longtblr document compiled with
// WithMaxPasses(1), which skips TeX rerun convergence. For documents that need
// no cross-reference resolution this roughly halves compilation time.
func BenchmarkCompileSinglePass(b *testing.B) {
	dir := fullBundleDir(b)
	ctx := context.Background()

	rows := 40
	if v := os.Getenv("TECGONIC_BENCH_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rows = n
		}
	}
	doc := longtblrDoc(rows)

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	b.ResetTimer()
	for b.Loop() {
		if _, err := c.CompileSource(ctx, doc, WithMaxPasses(1)); err != nil {
			b.Fatalf("Compile: %v", err)
		}
	}
}

// BenchmarkCompileWarmAux measures the longtblr document compiled with
// WithStateDir after an initial seeding compile. When the document's feedback
// data is stable, the seeded aux lets the engine converge after a single
// pass — the multi-pass safety net stays in place, unlike WithMaxPasses(1).
func BenchmarkCompileWarmAux(b *testing.B) {
	dir := fullBundleDir(b)
	ctx := context.Background()

	rows := 40
	if v := os.Getenv("TECGONIC_BENCH_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rows = n
		}
	}
	doc := longtblrDoc(rows)

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	stateDir := b.TempDir()
	// Seed the state with one cold compile outside the timer.
	if _, err := c.CompileSource(ctx, doc, WithStateDir(stateDir)); err != nil {
		b.Fatalf("Compile (seed): %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := c.CompileSource(ctx, doc, WithStateDir(stateDir)); err != nil {
			b.Fatalf("Compile: %v", err)
		}
	}
}

// BenchmarkCompileSimple measures a trivial document, capturing the fixed
// per-compile cost (module instantiation, format load, filesystem setup)
// rather than macro-expansion throughput.
func BenchmarkCompileSimple(b *testing.B) {
	dir := bundleDir(b)
	ctx := context.Background()

	doc := []byte(`\documentclass{article}
\begin{document}
Hello, World!
\end{document}
`)

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	b.ResetTimer()
	for b.Loop() {
		if _, err := c.CompileSource(ctx, doc); err != nil {
			b.Fatalf("Compile: %v", err)
		}
	}
}
