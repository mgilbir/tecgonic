package tecgonic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

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
	pdf, err := c.Compile(ctx, tex, WithStderr(&stderr))
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
		pdf, err := c.Compile(ctx, tex)
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

	_, err = c.Compile(ctx, tex)
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

	_, err = c.Compile(ctx, tex)
	if err == nil {
		t.Fatal("expected error when no bundle dir set, got nil")
	}

	t.Logf("Got expected error: %v", err)
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
			pdfs[i], errs[i] = c.Compile(ctx, tex)
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
	pdf, err := c.Compile(ctx, tex)
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
	if _, err := c2.Compile(ctx, tex); err == nil {
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
	pdf, err := c.Compile(ctx, tex, WithMaxPasses(1), WithStderr(&stderr))
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
	pdf, err := c.Compile(ctx, tex, WithStderr(&stderr))
	if err != nil {
		t.Fatalf("Compile: %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
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
	pdf1, err := c.Compile(ctx, tex, WithStateDir(stateDir), WithStderr(&stderr1))
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
	pdf2, err := c.Compile(ctx, tex, WithStateDir(stateDir), WithStderr(&stderr2))
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
	if _, err := c.Compile(ctx, v1, WithStateDir(stateDir)); err != nil {
		t.Fatalf("Compile (v1): %v", err)
	}

	// Ground truth: v2 compiled cold, no state involved.
	want, err := c.Compile(ctx, v2)
	if err != nil {
		t.Fatalf("Compile (v2 cold): %v", err)
	}

	// v2 compiled against v1's stale state must produce identical output.
	var stderr bytes.Buffer
	got, err := c.Compile(ctx, v2, WithStateDir(stateDir), WithStderr(&stderr))
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

	_, err = c.Compile(ctx, tex)
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
