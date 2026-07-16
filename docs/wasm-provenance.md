# The WASM module: provenance and updates

tecgonic embeds a single pre-built artifact, `wasm/tectonic_wasi.wasm` (~5 MB):
the Tectonic engine compiled to `wasm32-wasip1`. This page explains where that
module comes from, how the pieces stay in sync, and how to update it. (For the
*runtime* side — andsifr, our wazero fork that executes the module — see the
[performance doc](performance.md); that is a separate dependency from the engine
described here.)

## The chain

```
tectonic-typesetting/tectonic  (upstream engine, master)
        │  fork
        ▼
mgilbir/tectonic @ wasm         (adds crates/wasi: a WASI reactor build + host-contract patches)
        │  Makefile: TECTONIC_REF pins one commit
        ▼
make wasm  (Docker cross-compile)  ──►  wasm/tectonic_wasi.wasm   (committed here)
        │  embedded via wasm/embed.go
        ▼
New()  ──►  verifyABIVersion  ──►  refuses a module whose ABI ≠ expectedABIVersion
```

- **Upstream** is the unmodified [Tectonic](https://github.com/tectonic-typesetting/tectonic) engine.
- **The fork** (`mgilbir/tectonic`, branch `wasm`) adds only what a sandboxed,
  embeddable build needs: the `crates/wasi` reactor crate plus a small set of
  host-contract features (typed abort status, `malloc`/`free` and
  `tectonic_abi_version` exports, warm-aux/state export, `TECTONIC_MAX_PASSES`,
  and `SOURCE_DATE_EPOCH` for the document date). Its branch model, the full
  change ledger, and the upstream-sync procedure are documented in the fork's
  [`crates/wasi/README.md`](https://github.com/mgilbir/tectonic/blob/wasm/crates/wasi/README.md).
- **`TECTONIC_REF`** in the [Makefile](../Makefile) pins the exact fork commit the
  embedded module was built from, so the artifact is reproducible.
- **The ABI handshake** (`tectonic_abi_version` in the module ↔ `expectedABIVersion`
  in `tecgonic.go`) is the safety net: `New` rejects a module built from an
  incompatible source instead of letting it misbehave silently.

## Why a fork, and the maintenance cost

Upstream Tectonic has no `wasm32-wasip1` reactor build, no clock-free date path,
and no host ABI — it is built to run as a native CLI. The fork supplies those.
The delta is small and localized (a new crate plus cfg-gated build patches), so
tracking upstream is a periodic rebase, not a perpetual merge conflict; the
procedure lives in the fork README. The trade-off is the usual one for a fork:
security and correctness fixes from upstream Tectonic land here only when someone
rebases `wasm` and rebuilds.

## Updating the embedded module

When the fork changes (a new upstream rebase, or a host-contract change like the
date support):

1. **Land the fork change on `wasm`** and note the merged commit SHA.
2. **Bump `TECTONIC_REF`** in the [Makefile](../Makefile) to that SHA. Pin a SHA,
   not a branch name — a new SHA busts the Docker git-clone layer cache, whereas a
   branch keeps the same value and would reuse a stale clone.
3. **Rebuild:** `make wasm` (Docker) writes a fresh `wasm/tectonic_wasi.wasm`.
   (Maintainers with the toolchain can instead build locally from a fork checkout:
   `WASI_SDK_PATH=~/wasi-sdk ./build-wasi.sh`, then copy the artifact — byte
   provenance is documented by `TECTONIC_REF` either way.)
4. **If the module's ABI changed**, bump `expectedABIVersion` in `tecgonic.go` to
   match the module's new `tectonic_abi_version`. These two numbers move together;
   a mismatch is caught at `New`, by design.
5. **Test:** `go test ./...` (runs against the committed minibundle with no setup),
   and against a full bundle with `TECGONIC_BUNDLE_DIR` set for the heavier cases.

## The ABI contract

`expectedABIVersion` (see its godoc in `tecgonic.go`) covers the export names and
signatures, the reserved `proc_exit` abort status, the recognized environment
variables, and the guest mount layout. Any change to those on either side must
bump both numbers in the same change. Version history:

- **1** — typed `proc_exit` abort status; `TECTONIC_MAX_PASSES`.
- **2** — `SOURCE_DATE_EPOCH` drives `\today` and the PDF timestamp (see
  `WithBuildDate`); an ABI 1 module ignores it and renders 1970, so `New` rejects it.
