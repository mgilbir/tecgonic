# Performance

Compilation is CPU-bound. This page covers the three levers that trade
correctness headroom for speed — the context-cancellation cost, pass capping,
and state seeding — and the optimized wazero fork the engine runs on. For the
one-time startup cost (WASM module compilation) and its on-disk cache, see the
README's "WASM compilation cache" section.

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
pdf, err := compiler.CompileSource(ctx, tex, tecgonic.WithMaxPasses(1))
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
pdf, err := compiler.CompileSource(ctx, tex, tecgonic.WithStateDir(stateDir))
```

```bash
TECGONIC_BUNDLE_DIR=/path/to/bundle \
	go test -run '^$' -bench 'BenchmarkCompileLongtblr/cancellation_off|BenchmarkCompileSinglePass|BenchmarkCompileWarmAux' -benchtime=3x .
```

## WASM engine: andsifr (a wazero fork)

The WASM engine is [andsifr](https://github.com/mgilbir/andsifr), a fork of
[wazero](https://github.com/tetratelabs/wazero) with its own module path. It
carries compiler optimizations that target exactly tecgonic's workload — an
interpreter-style module whose state lives in globals at constant addresses:

- **Bounds-check elision for statically safe addresses**: accesses at
  constant addresses within the memory's declared minimum size can never be
  out of bounds (memories only grow), so no runtime check is emitted.
- **Shared trap islands** (arm64 and amd64): conditional traps branch to one
  shared per-function exit sequence and fall through on the hot path, instead
  of inlining ~10 instructions at every check site.

Measured on the 40-row `longtblr` benchmark document:

| CPU | stock wazero v1.12.0 | andsifr |
| --- | --- | --- |
| Apple M1 Pro | 16.4 s | **9.8 s (1.7x)** |
| Intel i7-3770 | 38.8 s | **19.3 s (2.0x)** |

Generated machine code for the module also shrinks from 18.2 MB to 10.8 MB.
The full wazero test suite and the WebAssembly spec tests pass on the fork on
both linux/amd64 and darwin/arm64.

Because andsifr is an ordinary dependency (not a `replace` directive),
applications importing tecgonic as a library get it automatically — no
changes to your `go.mod` are needed.
