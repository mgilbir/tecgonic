package tecgonic

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrorKind classifies why a Tectonic engine run failed. It is what callers
// building error handling should branch on: show a TeX error to the document
// author, alert on an engine failure, ignore a cancellation.
type ErrorKind int

const (
	// KindTexError means the input document is invalid; the captured Logs
	// explain what. This is the author's problem, not an engine fault.
	KindTexError ErrorKind = iota
	// KindEngine means the WASM engine itself failed: a trap, or an exit code
	// other than the expected TeX-error code. This is an operational fault.
	KindEngine
	// KindCancelled means the run was aborted by context cancellation or a
	// deadline being exceeded. Only observable when the compiler was created
	// with WithContextCancellation.
	KindCancelled
)

func (k ErrorKind) String() string {
	switch k {
	case KindTexError:
		return "tex-error"
	case KindEngine:
		return "engine-error"
	case KindCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// EngineError is a failure from running the Tectonic engine, returned by both
// Compile and GenerateFormat. Inspect Kind (or the Is* helpers) to decide how
// to react.
type EngineError struct {
	Kind     ErrorKind
	ExitCode int32  // engine exit code; 0 when the failure was a trap or a cancellation
	Logs     string // stderr captured from tectonic
	WasmErr  error  // underlying wazero error for traps and cancellations, nil otherwise
}

func (e *EngineError) Error() string {
	var kind string
	switch e.Kind {
	case KindTexError:
		kind = "TeX compilation error"
	case KindCancelled:
		kind = "compilation cancelled"
	default:
		kind = "Tectonic engine error"
	}
	if e.ExitCode != 0 {
		kind = fmt.Sprintf("%s (exit code %d)", kind, e.ExitCode)
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

// IsTexError reports whether the failure was a TeX compilation error (the
// document is invalid).
func (e *EngineError) IsTexError() bool { return e.Kind == KindTexError }

// IsEngineFailure reports whether the WASM engine itself faulted (a trap or an
// unexpected exit code), as opposed to a document error or a cancellation.
func (e *EngineError) IsEngineFailure() bool { return e.Kind == KindEngine }

// IsCancelled reports whether the run was aborted by context cancellation or a
// deadline. errors.Is(err, context.Canceled) / context.DeadlineExceeded also
// works via Unwrap.
func (e *EngineError) IsCancelled() bool { return e.Kind == KindCancelled }

// Unwrap returns the underlying wazero error for errors.Is/errors.As chaining.
func (e *EngineError) Unwrap() error {
	return e.WasmErr
}

// texAbortMarkers appear in tectonic's stderr when the engine deliberately
// aborts on a TeX error. A TeX error unwinds via a longjmp that surfaces to
// wazero as a WASM trap — indistinguishable by exit code from a genuine engine
// fault — so the log is the only reliable signal that the document, not the
// engine, is at fault. This couples classification to the WASM build's wording;
// the integration tests pin it.
var texAbortMarkers = []string{
	"TeX engine abort", // "fatal: longjmp called (TeX engine abort)"
	"tectonic error:",  // "tectonic error: input.tex:3: Undefined control sequence"
}

func isTexAbort(logs string) bool {
	for _, m := range texAbortMarkers {
		if strings.Contains(logs, m) {
			return true
		}
	}
	return false
}

// newEngineError classifies an engine run's outcome into an *EngineError.
// callErr is the error returned by module instantiation or the WASM call (a
// trap or a cancellation), or nil; exitCode is the engine's return value when
// callErr is nil. It must only be called on a genuine failure (callErr != nil,
// or a non-zero exit code).
func newEngineError(callErr error, exitCode int32, logs string) *EngineError {
	// Cancellation first: it surfaces as an error whose cause maps to a context
	// error (at module instantiation, or as a reserved WASM exit code during the
	// call). Detect it from the error, not from ctx.Err(), so a caller-cancelled
	// context does not mislabel a TeX error when cancellation is disabled.
	if callErr != nil && (errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded)) {
		return &EngineError{Kind: KindCancelled, Logs: logs, WasmErr: callErr}
	}
	// A TeX error aborts via a trap, so neither the exit code nor the presence of
	// a trap distinguishes a bad document from an engine fault; the
	// controlled-abort marker in the log does.
	if isTexAbort(logs) {
		return &EngineError{Kind: KindTexError, ExitCode: exitCode, Logs: logs}
	}
	return &EngineError{Kind: KindEngine, ExitCode: exitCode, Logs: logs, WasmErr: callErr}
}
