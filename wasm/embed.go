// Package wasm embeds the Tectonic engine compiled to WebAssembly.
package wasm

import _ "embed"

//go:embed tectonic_wasi.wasm
var module []byte

// Module returns the embedded Tectonic WASM module bytes. It returns a fresh
// copy on each call so callers cannot corrupt the shared embedded module.
func Module() []byte {
	out := make([]byte, len(module))
	copy(out, module)
	return out
}
