// Package tecgonic compiles LaTeX documents to PDF using the Tectonic engine
// compiled to WebAssembly, with no native TeX installation required.
//
// # Setup order
//
// The API does not enforce an order, but a working compiler is built in four
// steps, the first three of which are one-time:
//
//  1. PrepareBundle(ctx, dir, …) — download and extract the TeX Live bundle.
//     Skipped if dir already holds the requested bundle.
//  2. New(ctx, …) — create a Compiler, compiling the WASM module (~1.4 s, or
//     ~50 ms with WithCompilationCache).
//  3. Compiler.GenerateFormat(ctx, dir) — write latex.fmt into the bundle dir.
//     A no-op if it already exists.
//  4. Compiler.Compile / Compiler.CompileSource — compile a document. Each call
//     runs in its own isolated WASM instance and is safe for concurrent use.
//
// # Error handling
//
// Compile and GenerateFormat return an *EngineError whose Kind (or the Is*
// helpers) tells you how to react: route a KindTexError back to the document
// author, alert on a KindEngine operational fault, ignore a KindCancelled.
// KindCancelled is only observable when the compiler was created with
// WithContextCancellation. See ErrorKind for the full contract, including why a
// package missing from the bundle can surface as a KindTexError.
//
// # Untrusted input
//
// Compile serves the caller's fs.FS to the engine read-only and never writes it
// to the host, so the fs.FS is the document's entire input visibility and a
// trust boundary the caller owns. For hostile input, combine an in-memory fs.FS
// (os.DirFS follows symlinks out of its root), WithContextCancellation (to bound
// CPU), and WithMemoryLimitMiB (to bound WASM memory); bound disk use at the
// filesystem or container level.
//
// See the README for setup, performance tuning, and the andsifr runtime fork.
package tecgonic
