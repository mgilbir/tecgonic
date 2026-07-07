package tecgonic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mgilbir/andsifr/sys"
)

// texAbortExitCode is the WASI exit status the engine uses (via proc_exit) when
// it aborts a run through the fatal longjmp path (_tt_abort): a TeX error, an
// overflow, or another controlled engine abort. It is a contract with the WASM
// build (mgilbir/tectonic@wasm, wasi-deps/sjlj-stub/setjmp.c): a typed signal
// that replaces scraping stderr for the "TeX engine abort" marker. Older modules
// that abort via an untyped trap instead are still handled by the marker
// fallback in newEngineError.
const texAbortExitCode = 42

// ErrorKind classifies why a Tectonic engine run failed. It is what callers
// building error handling should branch on: show a TeX error to the document
// author, alert on an engine failure, ignore a cancellation.
type ErrorKind int

const (
	// KindTexError means tectonic reported a controlled abort while processing
	// the document; the captured Logs explain what. This is usually an invalid
	// document (the author's problem) — but the engine aborts through the same
	// channel for some environment faults it only discovers mid-run, most
	// notably a missing package in the bundle. Compile validates the bundle
	// directory and latex.fmt before running the engine, so those setup faults
	// surface as errors before classification; still, treat KindTexError as
	// "tectonic rejected this run" and confirm the environment is sound before
	// attributing every failure to the author. See IsTexError.
	KindTexError ErrorKind = iota
	// KindEngine means the WASM engine itself failed: a trap, an exit code other
	// than the expected TeX-error code, or a controlled abort caused by the
	// environment rather than the document (a format file or bundle input the
	// engine could not load). This is an operational fault.
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

// IsTexError reports whether tectonic aborted on a controlled TeX error, which
// usually means the document is invalid. It can also fire for an environment
// fault the engine only detects mid-run (e.g. a package missing from the
// bundle), so verify the bundle is the one the document expects before routing
// the failure back to the author as their mistake.
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
// aborts on a TeX error. They are the fallback signal for older WASM modules
// whose abort unwinds via a longjmp that surfaces to wazero as an untyped trap —
// indistinguishable by exit code from a genuine engine fault — so the log is the
// only signal. Current modules instead proc_exit with texAbortExitCode, which
// newEngineError prefers. This couples the fallback to the WASM build's wording;
// the integration tests pin it.
var texAbortMarkers = []string{
	"TeX engine abort", // "fatal: longjmp called (TeX engine abort)"
	"tectonic error:",  // "tectonic error: input.tex:3: Undefined control sequence"
}

// setupFailureMarkers identify tectonic's own diagnostic for an environment
// fault: a mount the engine expected is absent (a typo'd or unmounted bundle or
// fonts dir), so the format file or a bundle input cannot be opened. Such a fault
// is operational — it must classify as KindEngine and page on-call, not be blamed
// on the document author — so it takes precedence over texAbortMarkers. Compile
// pre-validates the bundle dir and latex.fmt, so this is a belt-and-braces
// backstop for faults that slip past that check (e.g. a mount whose top directory
// exists at validation but is incomplete, or GenerateFormat's own bundle dir).
//
// The marker is trusted only on a runtimeWarningPrefix line (see isSetupFailure).
// The engine reports these faults on its "tectonic warning:" channel, whereas a
// document can only influence "tectonic error: ... File `...' not found" lines —
// e.g. by \input-ing a file whose name embeds a marker. Matching a marker
// anywhere in the log (the previous behavior) let such a document forge an
// operational fault out of an ordinary document error, mislabeling its own
// KindTexError as KindEngine and paging on-call (audit 2026-07-07 C1).
var setupFailureMarkers = []string{
	"failed to find a pre-opened file descriptor", // a mount is absent (e.g. a typo'd bundle/fonts dir)
}

// runtimeWarningPrefix marks a line as one of tectonic's own runtime diagnostics,
// as opposed to a "tectonic error:" line whose text a document can influence.
const runtimeWarningPrefix = "tectonic warning:"

// markerTailBytes bounds how far back a trap-classification marker is trusted.
// The runtime writes "TeX engine abort" as the final line before a longjmp trap,
// so a genuine marker always lands in this tail; it narrows the window in which a
// document could forge the marker to relabel a different trap (audit C8).
const markerTailBytes = 8 << 10 // 8 KiB

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func isTexAbort(logs string) bool { return containsAny(logs, texAbortMarkers) }

// isSetupFailure reports whether the log shows an environment fault on tectonic's
// own warning channel. It requires a setup marker and runtimeWarningPrefix on the
// same line so a document cannot forge the marker through a controlled filename
// echoed on a "tectonic error:" line (audit 2026-07-07 C1).
func isSetupFailure(logs string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, runtimeWarningPrefix) && containsAny(line, setupFailureMarkers) {
			return true
		}
	}
	return false
}

// tail returns the last markerTailBytes of s.
func tail(s string) string {
	if len(s) > markerTailBytes {
		return s[len(s)-markerTailBytes:]
	}
	return s
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

	// Typed abort: the engine ends a controlled abort with proc_exit, which
	// wazero surfaces as a *sys.ExitError. texAbortExitCode means the TeX engine
	// aborted the run — a document error, unless the log shows the abort was an
	// environment fault (which the engine reaches through the same path). Any
	// other exit code is an engine fault. This is the deterministic successor to
	// the stderr-marker heuristic below, which now only serves older modules that
	// still abort via an untyped trap.
	var exitErr *sys.ExitError
	if errors.As(callErr, &exitErr) {
		if exitErr.ExitCode() == texAbortExitCode && !isSetupFailure(logs) {
			return &EngineError{Kind: KindTexError, ExitCode: int32(exitErr.ExitCode()), Logs: logs}
		}
		return &EngineError{Kind: KindEngine, ExitCode: int32(exitErr.ExitCode()), Logs: logs, WasmErr: callErr}
	}

	// An environment fault (unloadable format file, missing bundle mount) aborts
	// through the same channel as a bad document. Its marker wins over the TeX
	// markers so the failure is reported as the operational fault it is.
	if isSetupFailure(logs) {
		return &EngineError{Kind: KindEngine, ExitCode: exitCode, Logs: logs, WasmErr: callErr}
	}

	if callErr != nil {
		// A trap. A genuine TeX error unwinds via longjmp, which the runtime
		// reports as "TeX engine abort" on the last line before trapping; any
		// other trap (e.g. a document hitting WithMemoryLimitMiB) does not emit
		// it. Requiring that marker — and only trusting it in the log tail — keeps
		// a hostile document from relabelling its own out-of-memory kill as a
		// document error (audit C8).
		if strings.Contains(tail(logs), "TeX engine abort") {
			return &EngineError{Kind: KindTexError, ExitCode: exitCode, Logs: logs}
		}
		return &EngineError{Kind: KindEngine, ExitCode: exitCode, Logs: logs, WasmErr: callErr}
	}

	// A non-zero exit without a trap: tectonic printed a controlled error and
	// exited (e.g. a package missing from the bundle). Both markers apply here;
	// an exit code is not something a document can forge the way it can \typeout
	// arbitrary text into the trap path above.
	if isTexAbort(logs) {
		return &EngineError{Kind: KindTexError, ExitCode: exitCode, Logs: logs}
	}
	return &EngineError{Kind: KindEngine, ExitCode: exitCode, Logs: logs}
}
