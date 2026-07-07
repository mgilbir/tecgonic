package tecgonic

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
		{"unloadable format is engine fault", trap, 0, "tectonic warning: open of input latex failed\nfatal: longjmp called (TeX engine abort)\n", KindEngine},
		{"missing mount is engine fault", trap, 0, "tectonic warning: failed to find a pre-opened file descriptor for \"/bundle\"\nfatal: longjmp called (TeX engine abort)\n", KindEngine},
		{"oom trap without abort marker", trap, 0, "loading fonts\nwasm error: out of memory\n", KindEngine},
		{"forged marker then oom trap", trap, 0, forged, KindEngine},
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
