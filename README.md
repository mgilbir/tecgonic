# tecgonic

A Go library that compiles LaTeX documents to PDF using the [Tectonic](https://tectonic-typesetting.github.io/) engine compiled to WebAssembly. No native TeX installation required.

## Features

- Pure Go — the Tectonic engine runs as WASM via [wazero](https://wazero.io/) (no CGo)
- Self-contained bundle download — fetches the TeX Live bundle on first use
- Concurrent compilation — each `Compile` call gets its own isolated WASM instance
- WASM compilation cache — optional on-disk cache cuts startup from ~1.4 s to ~50 ms

## Quick start

```go
package main

import (
	"context"
	"os"

	"github.com/mgilbir/tecgonic"
)

func main() {
	ctx := context.Background()
	bundleDir := os.Getenv("HOME") + "/.cache/tecgonic/bundle"
	cacheDir := os.Getenv("HOME") + "/.cache/tecgonic/wasm-cache"

	// Download the TeX bundle (~800 MB, one-time).
	tecgonic.PrepareBundle(ctx, bundleDir, "", false, tecgonic.WithProgress(os.Stderr))

	// Create compiler and generate format file (one-time).
	compiler, _ := tecgonic.New(ctx,
		tecgonic.WithDefaultBundleDir(bundleDir),
		tecgonic.WithCompilationCache(cacheDir),
	)
	defer compiler.Close(ctx)
	compiler.GenerateFormat(ctx, bundleDir)

	// Compile LaTeX to PDF.
	pdf, _ := compiler.Compile(ctx, []byte(`\documentclass{article}
\begin{document}
Hello, World!
\end{document}
`))
	os.WriteFile("output.pdf", pdf, 0o644)
}
```

See [examples/simple](examples/simple) for a complete runnable example.

## WASM compilation cache

Creating a `Compiler` with `New()` involves compiling the Tectonic WASM module, which takes ~1.4 s. Pass `WithCompilationCache(dir)` to cache the compiled module on disk. Subsequent calls load the cached result in ~50 ms — a **~26x speedup**.

```go
compiler, err := tecgonic.New(ctx,
	tecgonic.WithDefaultBundleDir(bundleDir),
	tecgonic.WithCompilationCache("/path/to/cache"),
)
```

Benchmark results (AMD Ryzen 9 6900HX):

```
BenchmarkNew/NoCache       1   1360 ms/op   79 MB/op   117k allocs/op
BenchmarkNew/WithCache    22     51 ms/op  6.8 MB/op    31k allocs/op
```

The cache directory can be shared across processes. The first invocation populates the cache; all later invocations (including from different processes) read from it.

## Context cancellation and performance

By default, `Compile` runs at full speed but does **not** honor context
cancellation or deadlines once a compilation has started: the call runs to
completion regardless of the context, and keeps a CPU core busy until it
finishes.

This is deliberate. Making a running compilation interruptible requires wazero
to insert a termination check on every loop back-edge and function call (its
`WithCloseOnContextDone` option). For CPU-heavy documents — most notably large
`tabularray` `longtblr` tables, which do a lot of expl3 macro expansion per row
— that check dominates runtime. Measured on a ~100-row colored `longtblr` table:

| Mode | Time |
| --- | --- |
| Default (cancellation off) | **34 s** |
| `WithContextCancellation()` | 164 s (**~5× slower**) |

So context cancellation is opt-in. Enable it only when you need to abort or
time out long-running compilations (e.g. untrusted input that could loop), and
accept the per-iteration cost:

```go
compiler, err := tecgonic.New(ctx,
	tecgonic.WithDefaultBundleDir(bundleDir),
	tecgonic.WithContextCancellation(), // interruptible, ~5x slower on heavy docs
)
```

You can reproduce the comparison with:

```bash
TECGONIC_BUNDLE_DIR=/path/to/bundle TECGONIC_BENCH_ROWS=100 \
	go test -run '^$' -bench BenchmarkCompileLongtblr -benchtime=1x .
```

## TeX passes and WithMaxPasses

Tectonic reruns TeX until the document's cross-reference data (`.aux`)
converges, up to 6 passes. Since modern LaTeX always records feedback data in
the aux file (e.g. the total page count), every document compiles with at
least 2 full passes — each pass repeats the entire cost of the document.

If your documents don't use cross-references, citations, tables of contents,
or "page X of Y" counters, you can skip rerun detection and roughly halve
compilation time:

```go
pdf, err := compiler.Compile(ctx, tex, tecgonic.WithMaxPasses(1))
```

Measured on the 40-row `longtblr` benchmark document (M1 Pro): 17.4 s default
vs 9.0 s with `WithMaxPasses(1)`, with byte-identical output. Documents that
do need extra passes will render stale or missing references (`??`) when
capped too low, so this is per-call and opt-in.

If you can't know upfront whether a document needs multiple passes, use
`WithStateDir` instead: it persists the feedback files (`.aux`, `.toc`, …)
across `Compile` calls, so the first pass of the next compile starts from the
previous run's data. When the document's feedback is unchanged, the engine
proves convergence after a single pass and skips the rerun — same ~2× speedup
as `WithMaxPasses(1)`, but the multi-pass safety net stays in place: a stale
seed just triggers the usual reruns, never wrong output.

```go
// One state directory per logical document.
pdf, err := compiler.Compile(ctx, tex, tecgonic.WithStateDir(stateDir))
```

```bash
TECGONIC_BUNDLE_DIR=/path/to/bundle \
	go test -run '^$' -bench 'BenchmarkCompileLongtblr/cancellation_off|BenchmarkCompileSinglePass|BenchmarkCompileWarmAux' -benchtime=3x .
```

## Building the WASM module

The pre-built WASM artifact is included under `wasm/`. To rebuild it from the Tectonic source:

```bash
make wasm
```

This uses Docker to cross-compile Tectonic to `wasm32-wasip1`. See the [Dockerfile](Dockerfile) for details.

## Thanks

This project would not be possible without:

- [Tectonic](https://tectonic-typesetting.github.io/) — a modernized, complete, self-contained TeX/LaTeX engine. Tectonic does all the heavy lifting of turning LaTeX into PDF; tecgonic simply makes it callable from Go.
- [wazero](https://wazero.io/) — a zero-dependency WebAssembly runtime for Go. wazero makes it practical to embed the Tectonic WASM binary in a pure-Go library with no CGo and no external dependencies.