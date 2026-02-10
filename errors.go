package tecgonic

import "fmt"

// CompileError represents a failure during LaTeX compilation.
type CompileError struct {
	ExitCode int32  // 1=TeX error, 2=panic/trap
	Logs     string // stderr output captured from tectonic
	WasmErr  error  // underlying wazero error (for traps), nil for normal TeX errors
}

func (e *CompileError) Error() string {
	var kind string
	switch e.ExitCode {
	case 1:
		kind = "TeX compilation error"
	case 2:
		kind = "WASM engine panic/trap"
	default:
		kind = fmt.Sprintf("exit code %d", e.ExitCode)
	}

	msg := kind
	if e.WasmErr != nil {
		msg += ": " + e.WasmErr.Error()
	}
	if e.Logs != "" {
		msg += "\n--- tectonic output ---\n" + e.Logs
	}
	return msg
}

// IsTexError returns true if the error is a TeX compilation error (exit code 1).
func (e *CompileError) IsTexError() bool {
	return e.ExitCode == 1
}

// IsPanic returns true if the error is a WASM engine trap (exit code 2).
func (e *CompileError) IsPanic() bool {
	return e.ExitCode == 2
}

// Unwrap returns the underlying wazero error for errors.Is/errors.As chaining.
func (e *CompileError) Unwrap() error {
	return e.WasmErr
}
