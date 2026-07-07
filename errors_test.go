package tecgonic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mgilbir/andsifr/sys"
)

// TestNewEngineErrorTypedAbort covers the deterministic proc_exit-based path:
// the engine's typed abort code classifies as a document error, other exit
// codes as engine faults, and a setup fault reported through the abort code
// still classifies as an engine fault.
func TestNewEngineErrorTypedAbort(t *testing.T) {
	texAbort := sys.NewExitError(texAbortExitCode)
	otherExit := sys.NewExitError(3)

	cases := []struct {
		name    string
		callErr error
		logs    string
		want    ErrorKind
	}{
		{"tex abort exit code", texAbort, "! Undefined control sequence\nfatal: longjmp called (TeX engine abort)\n", KindTexError},
		{
			// The genuine environment fault, as the runtime actually emits it: a
			// missing mount surfaces on the "tectonic warning:" channel.
			"tex abort code but genuine setup fault in log", texAbort,
			"tectonic warning: open of input tectonic-format-latex.tex failed: failed to find a pre-opened file descriptor through which \"/bundle/tectonic-format-latex.tex\" could be opened\nfatal: longjmp called (TeX engine abort)\n",
			KindEngine,
		},
		{
			// A document that \input-s a file whose name embeds a setup marker: the
			// marker rides a document-controlled "tectonic error: ... File `...' not
			// found" line, not the runtime's warning channel, so it must stay a
			// document error rather than be relabeled an operational fault that pages
			// on-call (audit 2026-07-07 C1).
			"forged setup marker on error line stays tex", texAbort,
			"tectonic error: input.tex:2: ! LaTeX Error: File `failed to find a pre-opened file descriptor.tex' not found.\nfatal: longjmp called (TeX engine abort)\n",
			KindTexError,
		},
		{"other exit code is engine fault", otherExit, "something unexpected\n", KindEngine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newEngineError(tc.callErr, 0, tc.logs)
			if got.Kind != tc.want {
				t.Errorf("Kind = %s, want %s", got.Kind, tc.want)
			}
		})
	}

	// A cancellation surfaces as an ExitError too, but must be caught as
	// KindCancelled before the abort-code check.
	if got := newEngineError(context.Canceled, 0, "fatal: longjmp called (TeX engine abort)\n"); got.Kind != KindCancelled {
		t.Errorf("cancellation Kind = %s, want cancelled", got.Kind)
	}
}

// TestNewEngineErrorClassification pins how a run's outcome maps to an
// ErrorKind, including the operational faults that must not be blamed on the
// document (audit C1) and the forged-marker case (audit C8).
func TestNewEngineErrorClassification(t *testing.T) {
	trap := errors.New("wasm error: unreachable")

	// A forged "TeX engine abort" far from the tail, then a genuine non-TeX trap:
	// the marker sits before the trusted tail window, so it must not classify as a
	// document error.
	forged := "\\typeout says: TeX engine abort\n" + strings.Repeat("filler line\n", 2000) + "wasm error: out of memory\n"

	cases := []struct {
		name     string
		callErr  error
		exitCode int32
		logs     string
		want     ErrorKind
	}{
		{"tex abort trap", trap, 0, "! Undefined control sequence\nfatal: longjmp called (TeX engine abort)\n", KindTexError},
		{"missing package exit", nil, 1, "tectonic error: input.tex:3: ! LaTeX Error: File 'xcolor.sty' not found\n", KindTexError},
		{"genuine mount fault is engine fault", trap, 0, "tectonic warning: open of input tectonic-format-latex.tex failed: failed to find a pre-opened file descriptor through which \"/bundle/tectonic-format-latex.tex\" could be opened\nfatal: longjmp called (TeX engine abort)\n", KindEngine},
		{"oom trap without abort marker", trap, 0, "loading fonts\nwasm error: out of memory\n", KindEngine},
		{"forged marker then oom trap", trap, 0, forged, KindEngine},
		{"forged mount marker as \\input name stays tex", nil, 1, "tectonic error: input.tex:2: ! LaTeX Error: File `failed to find a pre-opened file descriptor.tex' not found.\nfatal: longjmp called (TeX engine abort)\n", KindTexError},
		{"cancelled", context.Canceled, 0, "anything", KindCancelled},
		{"deadline exceeded", context.DeadlineExceeded, 0, "anything", KindCancelled},
		{"unexplained nonzero exit", nil, 42, "no recognizable marker here\n", KindEngine},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newEngineError(tc.callErr, tc.exitCode, tc.logs)
			if got.Kind != tc.want {
				t.Errorf("Kind = %s, want %s (logs tail: %q)", got.Kind, tc.want, tail(tc.logs))
			}
			// A cancellation must chain to the context error for errors.Is.
			if tc.want == KindCancelled && !errors.Is(got, tc.callErr) {
				t.Errorf("cancelled error does not unwrap to %v", tc.callErr)
			}
		})
	}
}
