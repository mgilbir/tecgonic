# tecgonic

A Go library that compiles LaTeX documents to PDF using the [Tectonic](https://tectonic-typesetting.github.io/) engine compiled to WebAssembly. No native TeX installation required.

## Features

- Pure Go — the Tectonic engine runs as WASM via [wazero](https://wazero.io/) (no CGo)
- Self-contained bundle download — fetches the TeX Live bundle on first use
- Concurrent compilation — each `Compile` call gets its own isolated WASM instance

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

	// Download the TeX bundle (~800 MB, one-time).
	tecgonic.PrepareBundle(ctx, bundleDir, "", false, tecgonic.WithProgress(os.Stderr))

	// Create compiler and generate format file (one-time).
	compiler, _ := tecgonic.New(ctx, tecgonic.WithDefaultBundleDir(bundleDir))
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