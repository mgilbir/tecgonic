package tecgonic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// testBuildDate pins the compilation date so tests that compare PDF bytes across
// separate compiles are deterministic: without WithBuildDate each compile stamps
// the current host time, so two runs straddling a second boundary would differ.
var testBuildDate = time.Date(2024, time.January, 15, 12, 0, 0, 0, time.UTC)

func bundleDir(tb testing.TB) string {
	tb.Helper()
	if testBundleDir == "" {
		tb.Skip("no bundle available (testdata bundle failed to extract and TECGONIC_BUNDLE_DIR unset)")
	}
	return testBundleDir
}

func TestCompileSimple(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	tex := []byte(`\documentclass{article}
\begin{document}
Hello, World!
\end{document}
`)

	var stderr bytes.Buffer
	pdf, err := c.CompileSource(ctx, tex, WithStderr(&stderr))
	if err != nil {
		t.Fatalf("Compile: %v\nstderr: %s", err, stderr.String())
	}

	// PDF files start with %PDF-
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not look like a PDF (got %d bytes, prefix: %q)", len(pdf), pdf[:min(20, len(pdf))])
	}

	t.Logf("Generated PDF: %d bytes", len(pdf))
}

func TestCompileMultiple(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	for i := 0; i < 3; i++ {
		tex := []byte(`\documentclass{article}
\begin{document}
Test document number ` + string(rune('1'+i)) + `.
\end{document}
`)
		pdf, err := c.CompileSource(ctx, tex)
		if err != nil {
			t.Fatalf("Compile #%d: %v", i+1, err)
		}
		if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			t.Fatalf("Compile #%d: output is not a PDF", i+1)
		}
	}
}

func TestCompileError(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// Invalid TeX that should cause a compilation error
	tex := []byte(`\documentclass{article}
\begin{document}
\undefined_command_that_does_not_exist
\end{document}
`)

	_, err = c.CompileSource(ctx, tex)
	if err == nil {
		t.Fatal("expected error for invalid TeX, got nil")
	}

	var engErr *EngineError
	if !errors.As(err, &engErr) {
		t.Fatalf("expected *EngineError, got %T: %v", err, err)
	}
	// A malformed document must classify as a TeX error, not an engine fault,
	// so callers can route it back to the document author. If this fails with
	// KindEngine, the WASM longjmp stub is trapping on TeX errors (audit C8)
	// and the taxonomy is not trustworthy.
	if !engErr.IsTexError() {
		t.Errorf("invalid TeX classified as %s (exit code %d), want tex-error; WasmErr=%v",
			engErr.Kind, engErr.ExitCode, engErr.WasmErr)
	}

	t.Logf("Got expected EngineError (kind %s, exit code %d)", engErr.Kind, engErr.ExitCode)
}

// TestCompileForgedSetupMarkerStaysTexError is the end-to-end regression for
// audit 2026-07-07 C1: a document that references a missing file whose name
// embeds a setup-failure marker used to have its own TeX error relabeled as an
// operational KindEngine fault (which a service routes to on-call), because the
// engine echoes the marker into a "File `...' not found" line and the classifier
// matched setup markers anywhere in the log. The failure must stay a KindTexError
// routed back to the document author.
func TestCompileForgedSetupMarkerStaysTexError(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// Each name is a real setup-failure marker; \input of a missing file echoes it
	// verbatim into the engine's error line.
	for _, marker := range []string{
		"failed to find a pre-opened file descriptor",
		"open of input latex failed",
	} {
		t.Run(marker, func(t *testing.T) {
			tex := []byte("\\documentclass{article}\n\\begin{document}\n\\input{" + marker + "}\n\\end{document}\n")
			_, err := c.CompileSource(ctx, tex)
			if err == nil {
				t.Fatal("expected a compile error for the missing \\input, got nil")
			}
			var eng *EngineError
			if !errors.As(err, &eng) {
				t.Fatalf("expected *EngineError, got %T: %v", err, err)
			}
			if !eng.IsTexError() {
				t.Errorf("document error with a forged marker classified as %s, want tex-error; a document must not be able to forge an operational fault", eng.Kind)
			}
		})
	}
}

func TestGenerateFormat(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// GenerateFormat should be a no-op if latex.fmt already exists
	err = c.GenerateFormat(ctx, dir)
	if err != nil {
		t.Fatalf("GenerateFormat: %v", err)
	}

	// Verify latex.fmt exists
	if _, err := os.Stat(dir + "/latex.fmt"); err != nil {
		t.Fatalf("latex.fmt not found after GenerateFormat: %v", err)
	}
}

func TestNoBundleDir(t *testing.T) {
	ctx := context.Background()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	tex := []byte(`\documentclass{article}
\begin{document}
Hello
\end{document}
`)

	_, err = c.CompileSource(ctx, tex)
	if err == nil {
		t.Fatal("expected error when no bundle dir set, got nil")
	}

	t.Logf("Got expected error: %v", err)
}

// TestCompileValidatesEnvironment covers the config-shaped errors that catch a
// misconfigured environment before the engine runs (audit C6): a nonexistent
// bundle dir, a bundle dir with no latex.fmt, and a nonexistent fonts dir. None
// of these must surface as an *EngineError (which callers route to the author).
func TestCompileValidatesEnvironment(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	fsys := fstest.MapFS{"main.tex": &fstest.MapFile{Data: []byte("x")}}
	notEngineError := func(t *testing.T, err error) {
		t.Helper()
		var eng *EngineError
		if errors.As(err, &eng) {
			t.Errorf("environment fault surfaced as *EngineError: %v", err)
		}
	}

	t.Run("nonexistent bundle dir", func(t *testing.T) {
		_, err := c.Compile(ctx, fsys, "main.tex", WithBundleDir(filepath.Join(t.TempDir(), "nope")))
		if err == nil {
			t.Fatal("expected error for nonexistent bundle dir")
		}
		if !strings.Contains(err.Error(), "bundle directory") {
			t.Errorf("error does not mention the bundle directory: %v", err)
		}
		notEngineError(t, err)
	})

	t.Run("bundle dir without latex.fmt", func(t *testing.T) {
		_, err := c.Compile(ctx, fsys, "main.tex", WithBundleDir(t.TempDir()))
		if err == nil {
			t.Fatal("expected error for missing latex.fmt")
		}
		if !strings.Contains(err.Error(), "latex.fmt") {
			t.Errorf("error does not mention latex.fmt: %v", err)
		}
		notEngineError(t, err)
	})

	t.Run("nonexistent fonts dir", func(t *testing.T) {
		// A minimal fake bundle: validateBundleDir only requires latex.fmt, so the
		// check proceeds to the fonts dir without needing a real bundle.
		fakeBundle := t.TempDir()
		if err := os.WriteFile(filepath.Join(fakeBundle, "latex.fmt"), []byte("fmt"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := c.Compile(ctx, fsys, "main.tex",
			WithBundleDir(fakeBundle),
			WithFontsDir(filepath.Join(t.TempDir(), "nofonts")))
		if err == nil {
			t.Fatal("expected error for nonexistent fonts dir")
		}
		if !strings.Contains(err.Error(), "fonts directory") {
			t.Errorf("error does not mention the fonts directory: %v", err)
		}
		notEngineError(t, err)
	})
}

// TestABIVersion checks the cross-repo handshake: New succeeds against the
// embedded module (whose ABI matches), and verifyABIVersion rejects a version
// the package was not built for — the loud failure that guards against a WASM
// rebuilt from an incompatible tectonic source.
func TestABIVersion(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx) // no bundle needed; the ABI check runs inside New
	if err != nil {
		t.Fatalf("New should pass the ABI check against the embedded module: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// A version the package was not built for must be rejected clearly.
	if err := verifyABIVersion(ctx, c.runtime, c.compiled, expectedABIVersion+1); err == nil {
		t.Error("expected a mismatch error for the wrong expected version, got nil")
	} else if !strings.Contains(err.Error(), "ABI version") {
		t.Errorf("mismatch error does not mention the ABI version: %v", err)
	}

	// The real expected version verifies.
	if err := verifyABIVersion(ctx, c.runtime, c.compiled, expectedABIVersion); err != nil {
		t.Errorf("expected ABI %d to verify against the embedded module, got: %v", expectedABIVersion, err)
	}
}

func TestCompileConcurrent(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	pdfs := make([][]byte, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tex := []byte(fmt.Sprintf(`\documentclass{article}
\begin{document}
Concurrent document %d.
\end{document}
`, i))
			pdfs[i], errs[i] = c.CompileSource(ctx, tex)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Compile: %v", i, errs[i])
			continue
		}
		if !bytes.HasPrefix(pdfs[i], []byte("%PDF-")) {
			t.Errorf("goroutine %d: output is not a PDF", i)
		}
	}
}

func TestCompileMemoryLimit(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	tex := []byte(`\documentclass{article}
\begin{document}
Hello
\end{document}
`)

	// A generous limit does not disturb a normal compile.
	c, err := New(ctx, WithDefaultBundleDir(dir), WithMemoryLimitMiB(1024))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pdf, err := c.CompileSource(ctx, tex)
	_ = c.Close(ctx)
	if err != nil {
		t.Fatalf("Compile under a generous memory limit: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
	}

	// A tiny limit must actually cap memory and fail the compile rather than
	// being silently ignored.
	c2, err := New(ctx, WithDefaultBundleDir(dir), WithMemoryLimitMiB(16))
	if err != nil {
		t.Fatalf("New (tiny limit): %v", err)
	}
	defer func() { _ = c2.Close(ctx) }()
	if _, err := c2.CompileSource(ctx, tex); err == nil {
		t.Error("expected compile to fail under a 16 MiB memory limit, got nil")
	}
}

func TestCompileMaxPasses(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// A document with a label would normally trigger a second TeX pass.
	tex := []byte(`\documentclass{article}
\begin{document}
\section{One}\label{sec:one}
Hello.
\end{document}
`)

	var stderr bytes.Buffer
	pdf, err := c.CompileSource(ctx, tex, WithMaxPasses(1), WithStderr(&stderr))
	if err != nil {
		t.Fatalf("Compile: %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
	}
	if bytes.Contains(stderr.Bytes(), []byte("running TeX pass 2")) {
		t.Error("WithMaxPasses(1) did not prevent a second TeX pass")
	}
}

// TestCompileForwardRef is a regression test for the in-memory filesystem
// truncation bug: opening an output file preloaded the previous pass's bytes,
// so a rerun that shrank an intermediate (e.g. the .xdv once `??` is replaced
// by a resolved reference) left stale tail bytes behind and crashed
// xdvipdfmx. Any document with a forward reference used to trap here.
func TestCompileForwardRef(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	tex := []byte(`\documentclass{article}
\begin{document}
See section \ref{sec:later} for details.
\section{Later}\label{sec:later}
Content.
\end{document}
`)

	var stderr bytes.Buffer
	pdf, err := c.CompileSource(ctx, tex, WithStderr(&stderr))
	if err != nil {
		t.Fatalf("Compile: %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
	}
}

// TestCompileBuildDate covers WithBuildDate: a pinned date makes output
// reproducible (the engine has no real clock), and different dates yield
// different output, proving the date actually reaches the engine (issue #37,
// where \today rendered 1970 because the module never received a date).
func TestCompileBuildDate(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	tex := []byte(`\documentclass{article}
\begin{document}
Compiled \today.
\end{document}
`)
	d1 := time.Date(2021, time.March, 4, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2022, time.November, 9, 0, 0, 0, 0, time.UTC)

	// Same pinned date across two compiles → byte-identical PDF.
	a, err := c.CompileSource(ctx, tex, WithBuildDate(d1))
	if err != nil {
		t.Fatalf("compile a: %v", err)
	}
	b, err := c.CompileSource(ctx, tex, WithBuildDate(d1))
	if err != nil {
		t.Fatalf("compile b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("same WithBuildDate produced different PDFs; output is not reproducible")
	}

	// A different date must change the output, or the date never reached the engine.
	other, err := c.CompileSource(ctx, tex, WithBuildDate(d2))
	if err != nil {
		t.Fatalf("compile other: %v", err)
	}
	if bytes.Equal(a, other) {
		t.Error("different WithBuildDate produced identical PDFs; the build date is not applied")
	}
}

func TestCompileStateDir(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// A forward reference forces a second pass on a cold compile.
	tex := []byte(`\documentclass{article}
\begin{document}
See section \ref{sec:later} for details.
\section{Later}\label{sec:later}
Content.
\end{document}
`)

	stateDir := t.TempDir()

	var stderr1 bytes.Buffer
	pdf1, err := c.CompileSource(ctx, tex, WithStateDir(stateDir), WithBuildDate(testBuildDate), WithStderr(&stderr1))
	if err != nil {
		t.Fatalf("Compile (cold): %v\nstderr: %s", err, stderr1.String())
	}
	if !bytes.Contains(stderr1.Bytes(), []byte("running TeX pass 2")) {
		t.Errorf("cold compile should need a second pass, stderr: %s", stderr1.String())
	}
	if _, err := os.Stat(stateDir + "/input.aux"); err != nil {
		t.Fatalf("state dir has no input.aux after cold compile: %v", err)
	}

	var stderr2 bytes.Buffer
	pdf2, err := c.CompileSource(ctx, tex, WithStateDir(stateDir), WithBuildDate(testBuildDate), WithStderr(&stderr2))
	if err != nil {
		t.Fatalf("Compile (warm): %v\nstderr: %s", err, stderr2.String())
	}
	if bytes.Contains(stderr2.Bytes(), []byte("running TeX pass 2")) {
		t.Errorf("warm compile should converge in one pass, stderr: %s", stderr2.String())
	}
	if !bytes.Equal(pdf1, pdf2) {
		t.Error("warm compile produced different PDF than cold converged compile")
	}
}

// TestCompileStateDirStaleSeed verifies the safety property of WithStateDir:
// state left over from a previous version of the document must never change
// the output — a stale seed only costs extra passes.
func TestCompileStateDirStaleSeed(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	doc := func(sections string) []byte {
		return []byte(`\documentclass{article}
\begin{document}
See section \ref{sec:later} for details.
` + sections + `\section{Later}\label{sec:later}
Content.
\end{document}
`)
	}
	// v2 inserts a section before the label, so v1's recorded reference
	// data ("section 1") is stale for v2 (where it must resolve to 2).
	v1 := doc("")
	v2 := doc("\\section{Earlier}\n")

	stateDir := t.TempDir()

	// Populate the state dir with v1's feedback data.
	if _, err := c.CompileSource(ctx, v1, WithStateDir(stateDir), WithBuildDate(testBuildDate)); err != nil {
		t.Fatalf("Compile (v1): %v", err)
	}

	// Ground truth: v2 compiled cold, no state involved. Pin the date so the
	// byte comparison below reflects convergence, not the wall clock.
	want, err := c.CompileSource(ctx, v2, WithBuildDate(testBuildDate))
	if err != nil {
		t.Fatalf("Compile (v2 cold): %v", err)
	}

	// v2 compiled against v1's stale state must produce identical output.
	var stderr bytes.Buffer
	got, err := c.CompileSource(ctx, v2, WithStateDir(stateDir), WithBuildDate(testBuildDate), WithStderr(&stderr))
	if err != nil {
		t.Fatalf("Compile (v2 stale seed): %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.Equal(want, got) {
		t.Error("stale seed changed the compiled output")
	}
	// The stale seed must have been detected, forcing a rerun.
	if !bytes.Contains(stderr.Bytes(), []byte("running TeX pass 2")) {
		t.Errorf("expected a rerun with a stale seed, stderr: %s", stderr.String())
	}
}

func TestCompileContextCancel(t *testing.T) {
	dir := bundleDir(t)

	c, err := New(context.Background(), WithDefaultBundleDir(dir), WithContextCancellation())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	tex := []byte(`\documentclass{article}
\begin{document}
Hello
\end{document}
`)

	_, err = c.CompileSource(ctx, tex)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	// A caller-initiated cancellation must classify as cancelled, not as an
	// engine panic (audit C2): a timeout dashboard should not read as a crash.
	var engErr *EngineError
	if !errors.As(err, &engErr) {
		t.Fatalf("expected *EngineError, got %T: %v", err, err)
	}
	if !engErr.IsCancelled() {
		t.Errorf("cancelled compile classified as %s, want cancelled", engErr.Kind)
	}
	if engErr.IsEngineFailure() {
		t.Errorf("cancellation must not report as an engine failure")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; want true")
	}
	t.Logf("Got expected cancellation: %v", err)
}

func BenchmarkNew(b *testing.B) {
	ctx := context.Background()

	b.Run("NoCache", func(b *testing.B) {
		for b.Loop() {
			c, err := New(ctx)
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			_ = c.Close(ctx)
		}
	})

	b.Run("WithCache", func(b *testing.B) {
		cacheDir := b.TempDir()
		// Warm the cache with a single call outside the timer.
		c, err := New(ctx, WithCompilationCache(cacheDir))
		if err != nil {
			b.Fatalf("New (warm): %v", err)
		}
		_ = c.Close(ctx)

		b.ResetTimer()
		for b.Loop() {
			c, err := New(ctx, WithCompilationCache(cacheDir))
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			_ = c.Close(ctx)
		}
	})
}

// --- Multi-file input (fs.FS) feature tests ---

func TestCompileWithInputFS(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	fsys := fstest.MapFS{
		"paper.tex": &fstest.MapFile{Data: []byte(`\documentclass{article}
\usepackage{graphicx}
\begin{document}
\input{sections/content.tex}
\includegraphics[width=1cm]{images/pixel.png}
\end{document}
`)},
		"sections/content.tex": &fstest.MapFile{Data: []byte("Hello from an input FS.\n")},
		"images/pixel.png":     &fstest.MapFile{Data: onePixelPNG()},
	}

	var stderr bytes.Buffer
	pdf, err := c.Compile(ctx, fsys, "paper.tex", WithStderr(&stderr))
	if err != nil {
		t.Fatalf("Compile: %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output is not a PDF")
	}
}

func TestCompileMainInSubdir(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// The main source lives in a subdirectory; an \input alongside it resolves
	// relative to the main source's own directory. The output PDF name is
	// derived from the basename, so this produces "paper.pdf".
	fsys := fstest.MapFS{
		"src/paper.tex": &fstest.MapFile{Data: []byte(`\documentclass{article}
\begin{document}
\input{body.tex}
\end{document}
`)},
		"src/body.tex": &fstest.MapFile{Data: []byte("Main source in a subdirectory.\n")},
	}

	pdf, err := c.Compile(ctx, fsys, "src/paper.tex")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output is not a PDF")
	}
}

func TestCompileRejectsMissingMain(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	fsys := fstest.MapFS{"other.tex": &fstest.MapFile{Data: []byte("not the entry point")}}
	if _, err := c.Compile(ctx, fsys, "paper.tex"); err == nil {
		t.Fatal("expected error when main source is absent from input fs, got nil")
	}
}

func TestCompileRejectsInvalidMainName(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	fsys := fstest.MapFS{"main.tex": &fstest.MapFile{Data: []byte("x")}}

	// Invalid or non-resolving main names must be rejected: empty, the root
	// directory, parent escapes, and absolute paths all fail fs.Stat.
	for label, name := range map[string]string{
		"empty":         "",
		"directory":     ".",
		"parent escape": "../main.tex",
		"absolute":      "/main.tex",
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := c.Compile(ctx, fsys, name); err == nil {
				t.Fatalf("expected error for invalid main name %q, got nil", name)
			}
		})
	}
}

// TestCompileRejectsNonTexMain checks that a main source whose basename carries
// an extension other than .tex is rejected at the API boundary, rather than
// running the engine and failing late with "no PDF output was generated" from a
// jobname/output-name disagreement (audit C5).
func TestCompileRejectsNonTexMain(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	for _, name := range []string{"paper.ltx", "PAPER.TEX", "src/paper.latex", "doc.Tex"} {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{name: &fstest.MapFile{Data: []byte("x")}}
			_, err := c.Compile(ctx, fsys, name)
			if err == nil {
				t.Fatalf("expected rejection of non-.tex main %q, got nil", name)
			}
			if !strings.Contains(err.Error(), ".tex") {
				t.Errorf("error does not mention the .tex constraint: %v", err)
			}
			var eng *EngineError
			if errors.As(err, &eng) {
				t.Errorf("boundary rejection surfaced as *EngineError: %v", err)
			}
		})
	}
}

// TestCompileStateDirWithInputFS covers WithStateDir on a multi-file fs.FS with
// the main source in a subdirectory: state files are named after the main
// source's basename and overlaid next to it, without touching the caller's fs.
func TestCompileStateDirWithInputFS(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	fsys := fstest.MapFS{
		"src/paper.tex": &fstest.MapFile{Data: []byte(`\documentclass{article}
\begin{document}
See section \ref{sec:later} for details.
\input{body.tex}
\end{document}
`)},
		"src/body.tex": &fstest.MapFile{Data: []byte("\\section{Later}\\label{sec:later}\nContent.\n")},
	}

	stateDir := t.TempDir()

	var stderr1 bytes.Buffer
	pdf1, err := c.Compile(ctx, fsys, "src/paper.tex", WithStateDir(stateDir), WithBuildDate(testBuildDate), WithStderr(&stderr1))
	if err != nil {
		t.Fatalf("Compile (cold): %v\nstderr: %s", err, stderr1.String())
	}
	if !bytes.Contains(stderr1.Bytes(), []byte("running TeX pass 2")) {
		t.Errorf("cold compile should need a second pass, stderr: %s", stderr1.String())
	}
	// State files are named after the main source's basename.
	if _, err := os.Stat(stateDir + "/paper.aux"); err != nil {
		t.Fatalf("state dir has no paper.aux after cold compile: %v", err)
	}

	var stderr2 bytes.Buffer
	pdf2, err := c.Compile(ctx, fsys, "src/paper.tex", WithStateDir(stateDir), WithBuildDate(testBuildDate), WithStderr(&stderr2))
	if err != nil {
		t.Fatalf("Compile (warm): %v\nstderr: %s", err, stderr2.String())
	}
	if bytes.Contains(stderr2.Bytes(), []byte("running TeX pass 2")) {
		t.Errorf("warm compile should converge in one pass, stderr: %s", stderr2.String())
	}
	if !bytes.Equal(pdf1, pdf2) {
		t.Error("warm compile produced different PDF than cold converged compile")
	}
}

// onePixelPNG returns a minimal valid 1x1 PNG for the graphics input test.
func onePixelPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
		0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99,
		0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
		0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
}
