# Compiling untrusted input

tecgonic runs the TeX engine inside a WebAssembly sandbox, which makes it
practical to compile LaTeX you don't control — user uploads, API payloads,
generated documents. This page gathers the guarantees you get for free and the
knobs you must set yourself.

A hostile or pathological document has four levers: **filesystem access**,
**CPU/wall-clock**, **memory**, and **disk**. The sandbox closes the first by
construction; the other three are bounded by options you opt into.

## What the sandbox gives you for free

- **No host filesystem access.** The engine only sees the mounts tecgonic
  gives it: your input `fs.FS`, the read-only bundle, and per-compile temp
  directories. It cannot open arbitrary host paths.
- **No shell escape, no network.** The WASI build omits `\write18`/shell-escape
  and network bundle fetching entirely — a document cannot run a subprocess or
  make a network request.
- **The input `fs.FS` is read-only and never written to the host.** `Compile`
  serves it to the engine as the compilation root; there is no API by which the
  document can write back into it or onto the host.
- **Per-call isolation.** Every `Compile`/`CompileSource` runs in its own WASM
  instance with its own temp mounts, so concurrent compiles cannot see or
  corrupt each other.

## The four knobs you set

### 1. Filesystem — choose the right `fs.FS`

`Compile`'s `fsys` argument *is* the document's entire input visibility, so it
doubles as a trust boundary you own. The one sharp edge is **symlinks**:

- **Do not use `os.DirFS` for untrusted input.** It follows symbolic links,
  including ones that point outside its root — a document that references a
  symlink can read host files the engine can reach.
- **Prefer an in-memory `fs.FS`** (`fstest.MapFS`, an `embed.FS`, or the bytes
  you received) so there are no symlinks to follow. If you must serve a
  directory, use `fs.Sub` of a tree you have vetted.

```go
fsys := fstest.MapFS{
    "paper.tex": {Data: userUpload},
    // plus any assets you trust
}
pdf, err := compiler.Compile(ctx, fsys, "paper.tex")
```

For a single self-contained source, `CompileSource` stages the bytes in a
private temp dir — no caller `fs.FS` is involved.

### 2. CPU / wall-clock — `WithContextCancellation` + a deadline

By default a compilation runs to completion and keeps a CPU core busy until it
finishes: it does **not** honor context cancellation, so a document that loops
(`\loop`, runaway macro expansion) never returns. To bound it, create the
compiler with `WithContextCancellation()` and pass a deadline context to
`Compile`:

```go
compiler, _ := tecgonic.New(ctx,
    tecgonic.WithDefaultBundleDir(bundleDir),
    tecgonic.WithContextCancellation(), // makes compiles interruptible
)

ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()
pdf, err := compiler.Compile(ctx, fsys, "paper.tex")
// on timeout: errors.Is(err, context.DeadlineExceeded) — see Error handling below
```

This is opt-in because it is not free: wazero must insert a termination check on
every loop back-edge and function call, which is **~5× slower** on CPU-heavy
documents. Enable it precisely when you accept that cost to contain untrusted
input. (See [docs/performance.md](performance.md) for the measurement.)

### 3. Memory — `WithMemoryLimitMiB`

Without a cap, one compile can grow to the wasm32 ceiling of 4 GiB, so `N`
concurrent hostile documents can exhaust host memory. Cap it:

```go
compiler, _ := tecgonic.New(ctx,
    tecgonic.WithDefaultBundleDir(bundleDir),
    tecgonic.WithMemoryLimitMiB(512), // per-compile WASM memory ceiling (1..4096)
)
```

A document that exceeds the cap fails its compile (reported as a document
error — see below) rather than taking down the process.

### 4. Disk — bound it outside tecgonic

`WithMemoryLimitMiB` bounds **only** WASM linear memory. Each compile also writes
to on-disk temp directories (its output and cache mounts), which no option
limits. A document that emits a huge PDF or spams the cache can fill the disk.
Bound this at the layer below: a size-limited tmpfs, a container disk quota, or
filesystem quotas on the temp location.

## How hostile failures surface

A document that fails — bad syntax, a missing package, a hit memory cap, a
timeout — comes back as an `*EngineError`. The classification is driven by the
engine's own exit signal, **not** by anything the document writes to stderr, so
a document cannot forge how its failure is reported:

- `IsTexError()` → the document is at fault (or exhausted the memory cap). Return
  the logs to whoever submitted it.
- `IsCancelled()` (or `errors.Is(err, context.DeadlineExceeded)`) → it hit your
  timeout.
- `IsEngineFailure()` → an operational fault on your side (see the README's
  Error handling and Troubleshooting sections).

## Checklist

- [ ] Serve untrusted files through an in-memory `fs.FS` (or `fs.Sub` of a vetted
      tree) — never `os.DirFS`.
- [ ] `WithContextCancellation()` + a deadline context to bound CPU/wall-clock.
- [ ] `WithMemoryLimitMiB(...)` sized for your concurrency budget.
- [ ] A disk quota on the temp location (tmpfs size, container quota, or ulimit).
- [ ] Route `IsTexError()` back to the submitter; alert on `IsEngineFailure()`.

## See also

- README: Error handling and Troubleshooting.
- [docs/performance.md](performance.md): the context-cancellation cost, pass
  capping, and state seeding.
- Godoc for `Compile`, `WithMemoryLimitMiB`, and `WithContextCancellation`.
