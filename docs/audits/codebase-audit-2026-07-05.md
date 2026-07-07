# tecgonic — Adversarial Codebase Audit

**Date:** 2026-07-05
**Scope:** entire repository at commit `1c423a7` (`main`, clean tree). All Go source (~1,430 lines), README, Makefile, Dockerfile, examples, embedded-WASM packaging. The Rust/WASM side (github.com/mgilbir/tectonic branch `wasm`) and the andsifr fork internals were inspected only where load-bearing (error semantics traced into the andsifr module source).
**Method:** full read (no sampling); per-area expectation-vs-reality; every finding carries a concrete failure scenario and a CONFIRMED (traced in code) / PLAUSIBLE (mechanism traced, trigger not reproduced) verdict. Findings that did not survive an attempt at self-refutation were discarded (a list of these is at the end).

---

## 1. Summary table

| ID | Severity | Area | Issue | Location | Verdict |
|----|----------|------|-------|----------|---------|
| C1 | **High** | bundle lifecycle | Non-atomic extraction + `SHA256SUM`-presence as "done" marker → partial bundle permanently treated as complete | bundle.go:81–85 | CONFIRMED |
| C2 | Medium | errors | Context cancellation/timeout misreported as "WASM engine panic/trap" (`IsPanic()==true` for a user-initiated cancel) | tecgonic.go:315–321, errors.go:39–41 | CONFIRMED |
| C3 | Medium | concurrency | `latex.fmt` written non-atomically into a dir concurrently mounted read-only by `Compile` → torn format file | tecgonic.go:204 | CONFIRMED |
| C4 | Medium | supply chain | Downloaded bundle is never integrity-verified; `SHA256SUM` is extracted but unused | bundle.go:70–181 | CONFIRMED |
| C5 | Medium | bundle lifecycle | `force=true` / URL change re-extracts over old files without clearing; stale files and stale `latex.fmt` survive | bundle.go:81–89 | CONFIRMED |
| C6 | Medium | networking | `http.DefaultClient` with no timeout; README/example pass `context.Background()` → a stalled download hangs forever | bundle.go:97 | CONFIRMED |
| C7 | Medium | concurrency | `WithStateDir` writes state files non-atomically; concurrent compiles of the *same* document can seed torn `.aux`; doc warning covers only "different documents" | tecgonic.go:334–344, options.go:145 | PLAUSIBLE |
| C8 | Medium | errors | TeX-error (1) vs panic (2) taxonomy is unverified — the trap path suggests real TeX errors may surface as `IsPanic()`; the only error test never asserts exit code 1 | errors.go:7, tecgonic_test.go:95–105 | PLAUSIBLE |
| C9 | Low | API affordance | `GenerateFormat` ignores the compiler's `defaultBundleDir`; caller must re-pass the dir the compiler already knows | tecgonic.go:103–110 | CONFIRMED |
| C10 | Low | validation | `WithMaxPasses(0)` / negative values silently ignored despite documented "minimum 1" | options.go:126–130, tecgonic.go:295 | CONFIRMED |
| C11 | Low | docs | README quick start discards every error and uses `$HOME` (breaks on Windows); drifts from examples/simple which does it right | README.md:24–47 | CONFIRMED |
| C12 | Low | docs/validation | Extraction "validation" comment says "check for a common TeX file"; code counts ≥100 dir entries — including pre-existing unrelated files | bundle.go:171–178 | CONFIRMED |
| C13 | Low | unintended input | Basename flattening silently overwrites colliding entries; a tar entry named `..` or `/` yields a confusing EISDIR error (traversal itself is mitigated) | bundle.go:133–134 | CONFIRMED |
| C14 | Low | incoherence | `progressReader` mixes `atomic.Int64` for `read` with plain `int64` for `last` in single-goroutine code — implies concurrency that doesn't exist | bundle.go:34–40 | CONFIRMED |
| C15 | Low | resource bounds | Unbounded `stderrBuf`, unbounded `/output`+`/cache` writes, no WASM memory cap — untrusted-input story is incomplete beyond the documented CPU-loop case | tecgonic.go:273, 280–285 | CONFIRMED |
| C16 | Low | API surface | `wasm.TectonicWASM` is an exported mutable `[]byte`; any importer can corrupt the module before `New` | wasm/embed.go:6 | CONFIRMED |
| C17 | Low | DX | All tests skip silently without `TECGONIC_BUNDLE_DIR`; `make test` passes vacuously; the variable and test-bundle setup are documented nowhere; no CI | tecgonic_test.go:13–20, Makefile:9–10 | CONFIRMED |
| C18 | Low | reproducibility | Dockerfile clones un-pinned branch HEAD; Docker layer cache silently serves stale clones (`--no-cache` needed, undocumented) | Dockerfile:25 | CONFIRMED |
| C19 | Low | robustness | Format-file discovery scans only the cache top level and renames *any* `.fmt` to `latex.fmt` | tecgonic.go:182–197 | PLAUSIBLE |
| C20 | Low | completeness | `stateFileNames` omits beamer (`.nav`/`.snm`/`.vrb`) and index (`.idx`/`.ind`) feedback files → `WithStateDir` silently gives no speedup for those documents | tecgonic.go:23–25 | PLAUSIBLE |
| C21 | Low | API incoherence | `PrepareBundle(ctx, dir, "", false, ...)` uses positional sentinel params while everything else uses functional options; two ways to say "default URL" | bundle.go:70 | CONFIRMED |
| C22 | Low | naming | `GenerateFormat` returns `*CompileError` ("failure during LaTeX compilation") for a non-compilation operation | tecgonic.go:166–179, errors.go:5 | CONFIRMED |
| C23 | Low | DX | `go 1.25.5` toolchain floor undocumented; `make lint` assumes golangci-lint with no config committed | go.mod:3, Makefile:6–7 | CONFIRMED |
| C24 | Low | efficiency | Every tar entry is fully buffered in memory to trial gzip; a 2-byte magic sniff would avoid the double-buffering | bundle.go:137–149 | CONFIRMED |

**Counts:** 1 High, 7 Medium, 16 Low. No Critical findings — there is no unauthenticated network surface, no injection, and the WASM sandbox (mount-scoped FS, no shell escape) is genuinely sound.

---

## 2. System map

### Architecture

```
caller
  │  PrepareBundle(ctx, destDir, url, force)      ── HTTP GET → itar stream → flat destDir
  │  New(ctx, opts...)                            ── wazero(andsifr) runtime + compile embedded 5.2MB WASM
  │  Compiler.GenerateFormat(ctx, bundleDir)      ── one WASM run → writes latex.fmt INTO bundleDir
  │  Compiler.Compile(ctx, texSource, opts...)    ── per-call temp dir + fresh anonymous module instance
  ▼
wazero fork (andsifr)  ←  wasm/tectonic_wasi.wasm (embedded, built by Dockerfile from mgilbir/tectonic@wasm)
```

### Real execution path of `Compile` (tecgonic.go:213–367)

1. Merge per-call config over compiler defaults; error if no bundle dir (line 222).
2. `os.MkdirTemp` → `input/`, `output/`, `cache/` (+ empty `fonts/` if unset); `defer RemoveAll`.
3. Write `texSource` → `input/input.tex`; if `stateDir` set, seed `input.aux` etc. from it (lines 260–270).
4. Mount `/input` rw, `/output` rw, `/bundle` **ro**, `/fonts` rw, `/cache` rw; env `TECTONIC_FONT_DIR`, `TECTONIC_CACHE_DIR`, optional `TECTONIC_MAX_PASSES`. Anonymous module (`WithName("")`) → concurrent-safe.
5. `tectonic_compile_defaults()` → i32. Trap → `CompileError{ExitCode:2, WasmErr}`; non-zero → `CompileError{ExitCode:result}`.
6. On success: harvest state files from `output/` (best-effort), then read `output/input.pdf` (or stream to `cfg.output`).

### Key invariants and where they live

| Invariant | Enforced | Where |
|---|---|---|
| One WASM instance per compile; no shared mutable state | ✅ code | tecgonic.go:288, 300 |
| Bundle is read-only during compile | ✅ mount | tecgonic.go:283 |
| `bundleDir` contains a *complete* bundle **and a matching `latex.fmt`** | ❌ assumed, never checked | — (C1, C5) |
| `latex.fmt` is not being written while someone compiles | ❌ assumed | — (C3) |
| One `stateDir` per logical document, no concurrent writers | ⚠️ doc-only, and the doc understates it | options.go:145 (C7) |
| Exit code 1 = TeX error, 2 = engine panic | ⚠️ asserted in a comment, never tested | errors.go:7 (C8) |

The striking pattern: **everything inside a single `Compile` call is carefully isolated and correct; everything spanning calls, processes, or time (the bundle directory, the format file, the state directory) is assumed-consistent with no enforcement.** Most Medium+ findings are instances of this one gap.

### Onboarding path a newcomer actually follows

README → quick start (error-free pseudo-code, C11) → `examples/simple` (correct) → first run downloads ~800 MB with no timeout (C6) → `go test ./...` passes instantly because everything skipped (C17). Nothing tells them `TECGONIC_BUNDLE_DIR` exists or how to point it at an extracted bundle.

---

## 3. Findings by category

### 3.1 Correctness & partial failure

**C1 (High, CONFIRMED) — Partial bundle extraction is permanently mistaken for a complete one.**
`bundle.go:81–85`:

```go
if !force {
    if _, err := os.Stat(filepath.Join(destDir, "SHA256SUM")); err == nil {
        return nil
    }
}
```

Extraction streams ~134k files directly into `destDir` (bundle.go:118–165) with no staging dir, no manifest, and no atomic "done" marker — completion is inferred from the presence of one tar entry whose position in the archive the code doesn't control.

Two concrete failure scenarios:

- *Scenario A (order-dependent):* process crashes / power loss / `ctx` cancelled mid-extraction after the `SHA256SUM` entry has been written. Every subsequent `PrepareBundle` returns `nil` immediately. Compiles then fail deep inside TeX with missing-file errors that look like document bugs, not setup bugs. Whether this triggers depends on where `SHA256SUM` sits in the tar — undetermined (see Open questions).
- *Scenario B (order-independent, fully traced):* a bundle was previously extracted successfully, so `SHA256SUM` exists. User calls `PrepareBundle(..., force=true)` (or the download dies halfway through a re-extraction). The old `SHA256SUM` is never deleted first, so if the forced re-extraction fails partway, the directory now holds a *mixture of two bundles* and the next non-force call declares it complete. Nothing ever detects this.

The final `len(entries) < 100` check (bundle.go:176) only runs on the successful-extraction path, so it never catches either scenario.

*Direction:* extract into `destDir/.partial-<random>` (or track a `tecgonic-manifest.json`), and atomically rename / write the marker only after the tar reader returns `io.EOF`. Delete any existing marker before starting a forced re-extraction. This also gives C4 and C5 a home.

**C2 (Medium, CONFIRMED) — Cancellation is reported as an engine panic.**
With `WithContextCancellation()`, cancelling a running compile makes `fn.Call` return `*sys.ExitError{exitCode: 0xffffffff}` (verified in the andsifr source: `sys/error.go:12–15`, and its `Is()` maps that code to `context.Canceled`). `Compile` wraps *any* `callErr` as:

```go
return nil, &CompileError{ExitCode: 2, Logs: ..., WasmErr: callErr}   // tecgonic.go:316–321
```

So a deliberate `cancel()` produces an error whose message begins `"WASM engine panic/trap: module closed with context canceled"` and whose `IsPanic()` returns `true`. Same in `GenerateFormat` (tecgonic.go:167–173).

*Scenario:* a server enforces a 30 s deadline on untrusted documents — exactly the use case the README recommends `WithContextCancellation` for. Every timeout is logged/alerted as an engine panic; dashboards show a "crash" rate that is actually the timeout rate. Mitigation: `errors.Is(err, context.Canceled)` *does* work through `Unwrap() → sys.ExitError.Is`, but nothing tells users to check that before trusting `IsPanic()`, and the error string lies regardless.

*Direction:* before wrapping, check `errors.Is(callErr, context.Canceled/DeadlineExceeded)` and return the context error (optionally wrapped with logs) instead of a fake panic; or add a distinct `Cancelled` classification to `CompileError`.

**C3 (Medium, CONFIRMED) — `latex.fmt` is written non-atomically into a directory other calls read.**
`tecgonic.go:204`: `os.WriteFile(filepath.Join(bundleDir, "latex.fmt"), fmtData, 0o644)` — a direct multi-megabyte write, no temp-file+rename. `Compile` mounts the same `bundleDir` read-only (tecgonic.go:283) and the engine loads `latex.fmt` from it.

*Scenario:* N replicas share a bundle volume (the README explicitly advertises cross-process sharing for the WASM cache, inviting the same pattern for the bundle). Replica A boots, stats no `latex.fmt`, generates, and is midway through the write; replica B compiles and the engine reads a truncated format → trap or corrupt-format error, misreported as a panic (see C2). Also reachable in-process: `GenerateFormat` and `Compile` on separate goroutines. The stat-then-write also means two concurrent `GenerateFormat` calls both do the expensive generation (wasteful, plus interleaved final writes).

*Direction:* write to `bundleDir/.latex.fmt.tmp-<rand>` and `os.Rename`; rename is atomic on POSIX and makes readers see either old or new, never torn.

**C7 (Medium, PLAUSIBLE) — `WithStateDir`'s safety claim assumes well-formed state; torn writes can violate it.**
Harvest is `_ = os.WriteFile(stateDir/name, ...)` (tecgonic.go:341) — non-atomic. `options.go:143–145` claims "This is always correct: a stale seed … only causes the usual reruns, never wrong output" and warns only against sharing a dir "between concurrent Compile calls **of different documents**."

*Scenario:* two concurrent compiles of the *same* document share a state dir (the doc reads as if that's fine). Compile A harvests `input.aux` while compile B seeds it; B copies a half-written aux into its input. A truncated `.aux` is not "stale but well-formed" — a line cut inside `\newlabel{...}` is a TeX syntax error in the aux file, which can fail the compile rather than just cost a rerun. The window is small (hence PLAUSIBLE) but each compile writes six files sequentially, and the doc actively tells users the mechanism is "always correct."

*Direction:* atomic temp+rename per state file (cheap), and widen the doc warning to any concurrent sharing.

**C8 (Medium, PLAUSIBLE) — The 1-vs-2 exit-code taxonomy that `IsTexError`/`IsPanic` sell is unverified, and there's design history suggesting it's wrong.**
`errors.go:7` documents `1=TeX error, 2=panic/trap`. But the engine's longjmp stub is known to trap on at least some TeX error paths (wazero then returns the trap from `fn.Call`), which lands in the `callErr` branch → `ExitCode: 2`. If any real TeX errors surface as traps, `IsTexError()` returns `false` for them and callers branching on it (e.g. "show the user their LaTeX error" vs "page the on-call about an engine crash") misroute. Tellingly, `TestCompileError` (tecgonic_test.go:95–105) asserts only that *some* `*CompileError` comes back and logs the exit code without asserting it equals 1 — the one test that could pin the taxonomy down deliberately doesn't.

*Direction:* run the invalid-TeX fixture against the current WASM and assert `ExitCode == 1`; if traps do occur on TeX errors, either fix the WASM wrapper or stop advertising the distinction.

### 3.2 Boundary & safety

Where it's sound (stated once): the WASM sandbox is real — file access is limited to the five mounts, `\write18`/shell-escape doesn't exist in this build, path traversal in bundle extraction is neutralized by `filepath.Base` (a hostile `../../x` entry cannot escape `destDir`; even a literal `..` entry only produces an EISDIR error, not a write — traced), and there are no secrets in the repo (`.claude/settings.local.json` contains only permission patterns).

**C4 (Medium, CONFIRMED) — The 800 MB download is never integrity-checked.**
The bundle ships a `SHA256SUM` entry; the code extracts it as just another file and never verifies anything against it (no checksum of the tar, no per-file verification). HTTPS to `relay.fullyjustified.net` (a third-party mirror, bundle.go:16) is the *only* integrity layer. A truncated body that happens to end on a tar boundary extracts "successfully" (tar's EOF handling accepts a clean stream end); corruption inside an individually-gzipped entry is caught only for *that* entry, at write time — but a short read of the *outer* stream between entries just looks like EOF.

*Scenario:* CDN truncation at an entry boundary → `PrepareBundle` returns nil (if >100 files landed) → marker written → C1's permanent-partial state, this time with no crash involved.

*Direction:* after extraction, verify a sample (or all) files against the extracted `SHA256SUM`; at minimum compare received byte count to `Content-Length`.

**C5 (Medium, CONFIRMED) — Re-extraction never removes stale state; `latex.fmt` can silently mismatch the bundle.**
`force=true` extracts over the existing directory (bundle.go:87 `MkdirAll`, then per-file `os.Create`) without clearing it. Files present in the old bundle but absent from the new one persist. Worse: `latex.fmt` is not part of the tar — it's generated (tecgonic.go:204) — so after switching `bundleURL` to a newer bundle, the *old* format file survives and `GenerateFormat`'s existence check (tecgonic.go:113) makes regeneration a permanent no-op. A format dumped from v33 macros running against v34 files is exactly the kind of skew that produces obscure engine failures.

*Scenario:* user upgrades from `default_bundle_v33.tar` to a v34 URL with `force=true`; compiles now run a v33 `latex.fmt` against v34 `.sty` files. Nothing errors at setup time.

*Direction:* on force, clear the directory (or extract to a fresh staging dir per C1, which subsumes this); treat `latex.fmt` as derived state keyed to the bundle (delete it on re-extraction, or record the bundle URL/digest next to it).

**C6 (Medium, CONFIRMED) — No HTTP timeout anywhere on the bundle download.**
`http.DefaultClient.Do(req)` (bundle.go:97) has no `Timeout`; the request honors `ctx`, but the README quick start and `examples/simple` both pass `context.Background()`. A connection that stalls after the TCP handshake (or mid-body — 800 MB gives it plenty of opportunity) hangs `PrepareBundle` forever with no progress output (progress only prints on bytes actually read).

*Direction:* accept an `*http.Client` option, and/or document that callers should bound the context. An idle-timeout via a body-watchdog would fix the mid-body stall case that a plain deadline can't size correctly for slow links.

**C15 (Low, CONFIRMED) — Resource bounds stop at CPU.**
The README markets `WithContextCancellation` for "untrusted input that could loop", but a hostile document can also: grow `stderrBuf` without bound (tecgonic.go:273 — every byte of engine chatter is buffered in memory even when the caller supplied no `WithStderr`), write unboundedly to `/output` and `/cache` (rw mounts on the host temp dir → disk exhaustion), and allocate up to the 4 GiB wasm32 ceiling per concurrent compile (no `WithMemoryLimitPages`). None of these are hypothetical for a service compiling user LaTeX.
*Direction:* cap the stderr buffer (ring buffer or `io.LimitWriter`-style), document the disk/memory exposure alongside the CPU one, and consider exposing wazero's memory limit.

**C13 (Low, CONFIRMED) — Basename flattening silently drops colliding entries.**
`name := filepath.Base(header.Name)` (bundle.go:133) means two tar entries with the same basename in different directories flatten to one path; the second overwrites the first with no log. Correct for tectonic's intentionally-flat itar, wrong-by-silence for any custom `bundleURL` (a parameter the API explicitly offers) whose tar has structure.
*Direction:* count/log overwrites, or reject duplicate basenames for non-default URLs.

### 3.3 Incoherences & affordance mismatches

**C9 (Low, CONFIRMED) — `GenerateFormat` ignores the default bundle dir the compiler was built with.**
`New(ctx, WithDefaultBundleDir(dir))` then `c.GenerateFormat(ctx, dir)` — the same dir passed twice, and passing a *different* dir than the compiler's default is an easy way to generate a format the compiler will never use. `Compile` falls back to `c.config.defaultBundleDir` (tecgonic.go:215); `GenerateFormat` has no such fallback (tecgonic.go:108) despite living on the same receiver. *Direction:* empty `bundleDir` → use the default; error only if both unset.

**C21 (Low, CONFIRMED) — `PrepareBundle`'s signature fights the package's own style.**
Everything else uses functional options; `PrepareBundle(ctx, destDir, bundleURL string, force bool, opts ...)` uses a positional `""`-means-default URL and a positional bool. Call sites read as `PrepareBundle(ctx, dir, "", false, ...)` — the README's own example includes the cryptic `"", false`. There are also two ways to request the default URL (`""` or `DefaultBundleURL`). *Direction:* `WithBundleURL(...)`/`WithForce()` options; keep the old signature as a deprecated wrapper if compatibility matters.

**C22 (Low, CONFIRMED) — `CompileError` is returned by non-compilation code paths.**
Its doc says "a failure during LaTeX compilation" (errors.go:5), but `GenerateFormat` returns it for format-generation failures (tecgonic.go:167–179). Callers matching `errors.As(err, &compErr)` around `Compile` and around `GenerateFormat` get the same type with the same misclassification issues as C2. Minor, but it's the type users will build error UX on. *Direction:* rename conceptually to an engine-error type, or document that `GenerateFormat` returns it too.

**C10 (Low, CONFIRMED) — `WithMaxPasses` silently ignores out-of-range values.**
Doc says "(minimum 1)" (options.go:115) but `WithMaxPasses(0)` and negative values just skip the env var (tecgonic.go:295 `if cfg.maxPasses > 0`) — the caller believes they capped passes and got the default 6. A config wired from user input (`maxPasses: 0` in YAML meaning "unset"… or a bug) never errors. *Direction:* clamp to 1 or return/panic on invalid values at option-application time.

**C14 (Low, CONFIRMED) — `progressReader` half-pretends to be concurrent.**
`read` is `atomic.Int64`, `last` is a plain field mutated in the same `Read` (bundle.go:38–39, 45–48). Either both need protection or neither does (it's single-goroutine: the tar reader is the only caller). As written it signals a concurrency contract that doesn't exist and would be a real race if anyone believed the signal. *Direction:* drop the atomic.

**C16 (Low, CONFIRMED) — `wasm.TectonicWASM` is exported mutable state.**
Any package in the process can do `wasm.TectonicWASM[0] = 0` before `New`, turning module compilation errors into an action-at-a-distance mystery. `//go:embed` requires a var, but it can live unexported behind a getter. *Direction:* unexport, expose `func Module() []byte` (or just have tecgonic import an internal package).

**C19 (Low, PLAUSIBLE) — Format discovery is loose in both directions.**
tecgonic.go:182–197: if `cache/latex.fmt` is missing it takes the *first* `*.fmt` at the cache top level and installs it *as* `latex.fmt`. Non-recursive (a module writing `cache/formats/latex.fmt` would be missed → hard error with logs, acceptable) and name-erasing (an `xelatex.fmt` would be silently rebranded `latex.fmt`). Works with today's WASM; it's an implicit cross-repo contract with no test pinning it.

**C20 (Low, PLAUSIBLE) — The state-file list quietly excludes whole document classes.**
`stateFileNames` (tecgonic.go:23–25) covers `.aux/.toc/.lof/.lot/.out/.bbl`. Beamer's `.nav/.snm/.vrb`, `makeidx`'s `.idx/.ind`, and glossaries' files aren't round-tripped, so `WithStateDir` silently provides zero speedup (never wrong output — the safety property holds) for beamer decks, exactly the long-compile documents users would reach for it on. Behavior also depends on which files the WASM module exports to `/output` — a second implicit cross-repo contract. *Direction:* harvest `output/input.*` by exclusion (everything except `.pdf/.log/.xdv`) rather than by allowlist, or document the list's limits on `WithStateDir`.

### 3.4 Documentation

**C11 (Low, CONFIRMED) — The README quick start is the dangerous easy path.**
README.md:24–47: `tecgonic.PrepareBundle(...)` return ignored; `compiler, _ := tecgonic.New(...)`; `pdf, _ := compiler.Compile(...)`; `os.WriteFile` unchecked; `os.Getenv("HOME")` (empty on Windows → paths rooted at `/.cache`). If the 800 MB download fails, this program nil-pointer-panics on `compiler.Compile` or writes a 0-byte PDF. Meanwhile `examples/simple/main.go` handles every error and uses `os.UserCacheDir` — the two "first contact" artifacts contradict each other. *Direction:* make the README snippet the example's shape (or literally include-by-reference), errors and all.

**C12 (Low, CONFIRMED) — Extraction-validation comment describes code that doesn't exist.**
bundle.go:171 says `// Validate that extraction produced files (check for a common TeX file)`; the code checks `len(entries) >= 100` — no specific file is checked, and pre-existing unrelated files in `destDir` count toward the threshold (point `destDir` at any populated directory and validation passes with zero extracted files if the tar was empty). *Direction:* check for a sentinel that must exist in every bundle (e.g. `latex.ltx`), and count only files written this run.

**Other doc gaps (folded into C17):** `TECGONIC_BUNDLE_DIR` and `TECGONIC_BENCH_ROWS` appear in README bench commands (README.md:104, 142) with no explanation of how to produce a qualifying directory (extract bundle + generate `latex.fmt`); `package tecgonic` has no package-level godoc; the `force` parameter's semantics (and C5's caveats) are undocumented.

Where docs are sound (once): the performance documentation is unusually good — `WithContextCancellation`'s 5× cost, `WithMaxPasses`'s staleness risk, and `WithStateDir`'s convergence mechanics are all accurate to the code, the benchmark names in README commands all exist, and the numbers quoted in options.go and README agree.

### 3.5 Developer experience

**C17 (Low, CONFIRMED) — The test suite passes on a fresh clone by testing nothing.**
Every functional test begins `dir := bundleDir(t)` → `t.Skip` when `TECGONIC_BUNDLE_DIR` is unset (tecgonic_test.go:13–20). `make test` = `go test ./...` → all skips → exit 0. A contributor (or CI, if it existed — there is no CI config in the repo) gets a green run having exercised zero lines of `Compile`. Nothing in README or Makefile explains how to set the variable up, and the knowledge currently lives only in tribal memory. *Direction:* a `make test-setup` target (PrepareBundle+GenerateFormat into a cache dir) plus a README "Development" section; make `make test` print a loud warning when everything skipped; add CI that provisions the bundle once and caches it.

**C18 (Low, CONFIRMED) — `make wasm` is non-reproducible two different ways.**
Dockerfile:25 clones `--branch wasm` at whatever HEAD is that day (no commit pin), so the embedded artifact can't be regenerated bit-for-bit. And because the clone is a cached layer, rebuilding after pushing upstream changes silently uses the *old* clone unless the builder knows to pass `--no-cache` — a footgun that has already bitten in this project's history and is documented nowhere. Also: the Makefile target is `build-wasm` while README says `make wasm` (README.md:180) — the documented command **fails** (`make: *** No rule to make target 'wasm'`). *Direction:* pin a commit SHA via `ARG TECTONIC_REF` (busts the cache when changed, fixing both problems), and fix the README target name.

**C23 (Low, CONFIRMED) — Toolchain requirements are implicit.**
`go 1.25.5` in go.mod (a very recent floor — needed for `b.Loop()` etc.) is mentioned nowhere in README; `make lint` requires golangci-lint with no version or config file committed, so contributors lint against whatever their global config is.

**C24 (Low, CONFIRMED) — Per-entry full buffering during extraction.**
bundle.go:137 `io.ReadAll(tr)` buffers each entry in memory solely so gzip can be *attempted*. Large entries (fonts, the format-sized files) spike allocations, ~134k times. Sniffing the 2-byte gzip magic (`0x1f 0x8b`) from a `bufio.Reader.Peek` would stream everything. Efficiency-only; correctness is fine.

---

## 4. Design tensions

**T1 — The bundle directory is a shared mutable database with no schema, versioning, or locking.**
Three actors write to or depend on `bundleDir`: `PrepareBundle` (creates ~134k files + `SHA256SUM`), `GenerateFormat` (adds derived `latex.fmt`), and `Compile` (mounts it read-only and trusts total consistency). There is no manifest, no version stamp, no lock, no atomicity — C1, C3, C4, C5 are all symptoms of this one absence. *Alternative to weigh:* a `tecgonic.json` manifest written atomically at the end of extraction (bundle URL, digest, file count, format-file digest), checked by `Compile`/`GenerateFormat`; staging-dir + rename for extraction; `flock` for cross-process generate. This converts four findings into one ~100-line change and makes "is this bundle usable?" answerable.

**T2 — The error model is a leaky translation of WASM exit mechanics, not a model of what callers must distinguish.**
Callers of a LaTeX service need exactly three verdicts: *your document is wrong* (show TeX log), *the engine failed* (page someone), *you cancelled it* (do nothing). `CompileError` encodes instead *how the failure physically surfaced* (return code vs trap), which collapses cancellation into panic (C2), possibly TeX errors into panic (C8), and format-generation failures into "compilation" errors (C22). *Alternative:* an explicit `Kind` enum (TexError / EngineError / Cancelled) computed at the wrap site by inspecting `sys.ExitError` and the return code, with `IsTexError`/`IsPanic` kept as compatibility shims over it.

**T3 — Correctness-critical contracts live in another repository, enforced by nothing here.**
The Go side hardcodes: exported function names, the exit-code meanings, the six state-file names the WASM "exports to /output", the `TECTONIC_MAX_PASSES` env protocol, the string `"running TeX pass 2"` (asserted in tests!), and the format file's landing spot in `/cache`. All of these are behaviors of `mgilbir/tectonic@wasm` — a branch the Dockerfile doesn't even pin (C18). A rebuild of the WASM can silently break `WithStateDir` or `WithMaxPasses` while every unit test still passes (they skip, C17). *Alternative:* a versioned handshake — export a `tecgonic_abi_version()` from the WASM and check it in `New`; pin the source commit; keep one integration test per contract point that CI actually runs.

**T4 — The performance story rides on a personal fork of the WASM runtime.**
andsifr delivers a real, measured 1.7–2× win, and the no-`replace` packaging is clean. But every wazero security fix and correctness patch now requires a manual merge into a fork with a bus factor of one, and consumers get it transitively without knowing. The README is admirably transparent about the fork's existence but silent about the maintenance contract. *Alternative to weigh:* keep the fork but track upstream tags mechanically (CI job that rebases and runs the spec suite), and consider a build tag or module variant that lets consumers opt back into stock wazero when they prefer patch-currency over speed.

**T5 — Per-call temp-dir plumbing treats the host filesystem as the only IPC with the module.**
Every compile does: MkdirTemp → write source → (seed state) → engine writes PDF to disk → Go reads it back → RemoveAll. That's fine at current scale, but it doubles I/O on the PDF (disk write + read even in the `WithOutput` streaming case), leaves temp-dir turds if the process is SIGKILLed mid-compile, and makes disk the ambient failure domain (C15's exhaustion). *Alternative:* wazero supports custom `fs.FS` mounts; an in-memory FS for `/input`+`/output` would eliminate the round trip and the cleanup — worth weighing against the debuggability of real directories.

---

## 5. Expectation gaps ("expected X, found Y")

| # | Expected (from name/docs/shape) | Found |
|---|---|---|
| G1 | `PrepareBundle` returning nil ⇒ bundle is usable | ⇒ only that a file named `SHA256SUM` exists (C1/C4/C5) |
| G2 | `IsPanic()==true` ⇒ engine crashed | Also true for your own `cancel()` and for `GenerateFormat` failures (C2/C22) |
| G3 | `WithContextCancellation` = "the" untrusted-input defense (README framing) | Only CPU is bounded; memory, disk, log growth are not (C15) |
| G4 | `Compiler` knows its bundle dir ⇒ `GenerateFormat` uses it | Must re-pass it positionally (C9) |
| G5 | `WithMaxPasses(0)` errors or means 0 | Silently means "default 6" (C10) |
| G6 | "Do not share a state dir between … different documents" ⇒ same document is fine | Same-document concurrency can tear state files (C7) |
| G7 | README quick start = working production-shaped code | Discards every error; breaks on Windows; contradicts the shipped example (C11) |
| G8 | `make test` green ⇒ library works | All functional tests skipped without an undocumented env var (C17) |
| G9 | `make wasm` (per README) rebuilds the artifact | Target doesn't exist (`build-wasm`), and even that isn't reproducible (C18) |
| G10 | Comment: "check for a common TeX file" | Code: count ≥100 directory entries, pre-existing ones included (C12) |
| G11 | `WithStateDir` speeds up any multi-pass document | Only documents whose feedback lives in the six hardcoded files; beamer gets nothing, silently (C20) |

---

## 6. Open questions (not resolvable from this repo)

1. **Where does `SHA256SUM` sit in the v33 itar?** If last, C1's Scenario A is largely defused (Scenario B stands regardless); if early, interrupted first-time extractions are silently poisonous. Answerable with one ranged GET against the bundle or a look at tectonic's bundle-builder.
2. **Do real TeX errors exit with code 1, or do some trap through the longjmp stub?** (C8). Needs a run against an extracted bundle; the current WASM's behavior decides whether `IsTexError` is trustworthy.
3. **Does `tectonic_compile_defaults` run the bibtex pass at all?** If not, harvesting `input.bbl` in `stateFileNames` is dead weight and bibliography documents can never converge warm.
4. **Does the shipped `wasm/tectonic_wasi.wasm` actually honor `TECTONIC_MAX_PASSES`?** The option docs hedge ("older modules ignore this"), and since the artifact isn't traceable to a source commit (C18), the repo itself can't answer.
5. **What is the intended support story for `relay.fullyjustified.net`?** A hardcoded third-party mirror as the sole default distribution point is an availability and (absent C4's verification) integrity dependency the project doesn't control.
6. **Is cross-process sharing of the bundle dir an intended use case?** The README advertises it for the WASM cache; whether the bundle dir is meant to be shared decides how seriously to take C3's locking direction.

---

## Appendix: candidate findings discarded after self-refutation

- *"examples/simple/output.pdf committed to the repo"* — it exists on disk but is untracked (`.gitignore` covers it; verified via `git ls-files`).
- *"Zip-slip path traversal in bundle extraction"* — `filepath.Base` neutralizes directory components; a literal `..`/`/` entry resolves to a directory and `os.Create` fails with EISDIR rather than writing (kept only as the C13 UX note).
- *"`errors.Is(err, context.Canceled)` broken by the CompileError wrap"* — it works: `Unwrap()` returns the `sys.ExitError`, whose `Is()` maps the reserved exit code to `context.Canceled` (verified in andsifr source). The C2 finding is therefore misclassification/misleading text, not undetectability.
- *"`Compiler.Close` drops the cache-close error"* — it doesn't; the cache is always closed and its error surfaces when the runtime closed cleanly (tecgonic.go:90–98). Error handling in `New`'s failure paths also correctly releases both runtime and cache.
- *"Concurrent `Compile` on one `Compiler` races"* — refuted; anonymous module instances (`WithName("")`) with per-call temp dirs are correctly isolated, and `TestCompileConcurrent` covers it.
- *"README benchmark commands reference nonexistent benchmarks"* — all three names exist with matching sub-benchmark names; the 34 s/164 s ≈ 5× arithmetic checks out.
