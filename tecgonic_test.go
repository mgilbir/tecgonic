package tecgonic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/fstest"
)

func bundleDir(tb testing.TB) string {
	tb.Helper()
	dir := os.Getenv("TECGONIC_BUNDLE_DIR")
	if dir == "" {
		tb.Skip("TECGONIC_BUNDLE_DIR not set")
	}
	return dir
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
		t.Fatalf("CompileSource: %v\nstderr: %s", err, stderr.String())
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
			t.Fatalf("CompileSource #%d: %v", i+1, err)
		}
		if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			t.Fatalf("Compile #%d: output is not a PDF", i+1)
		}
	}
}

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

	pdf, err := c.Compile(ctx, fsys, "paper.tex")
	if err != nil {
		t.Fatalf("Compile: %v", err)
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

	// The main source lives in a subdirectory; an \input alongside it is
	// referenced relative to the main source's own directory. The output PDF
	// name is derived from the basename, so this resolves to "paper.pdf".
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

	fsys := fstest.MapFS{
		"other.tex": &fstest.MapFile{Data: []byte("not the entry point")},
	}
	_, err = c.Compile(ctx, fsys, "paper.tex")
	if err == nil {
		t.Fatal("expected error when main source is absent from input fs, got nil")
	}
	t.Logf("Got expected error: %v", err)
}

func TestCompileRejectsInvalidMainName(t *testing.T) {
	dir := bundleDir(t)
	ctx := context.Background()

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	fsys := fstest.MapFS{
		"main.tex": &fstest.MapFile{Data: []byte("x")},
	}

	// The main name is rejected purely because it does not resolve to a file in
	// fsys: an empty name and invalid fs paths (parent escapes, absolute paths)
	// fail fs.Stat, and "." resolves to the root directory. Path validity is the
	// fsys provider's responsibility, so a subdirectory component (e.g.
	// "chapters/main.tex") is now allowed.
	cases := map[string]string{
		"empty":         "",
		"directory":     ".",
		"parent escape": "../main.tex",
		"absolute":      "/main.tex",
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := c.Compile(ctx, fsys, name)
			if err == nil {
				t.Fatalf("expected error for invalid main name %q, got nil", name)
			}
		})
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

	var compErr *CompileError
	if !errors.As(err, &compErr) {
		t.Fatalf("expected *CompileError, got %T: %v", err, err)
	}

	t.Logf("Got expected CompileError (exit code %d): %s", compErr.ExitCode, compErr.Logs)
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
	pdf1, err := c.CompileSource(ctx, tex, WithStateDir(stateDir), WithStderr(&stderr1))
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
	pdf2, err := c.CompileSource(ctx, tex, WithStateDir(stateDir), WithStderr(&stderr2))
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
	if _, err := c.CompileSource(ctx, v1, WithStateDir(stateDir)); err != nil {
		t.Fatalf("Compile (v1): %v", err)
	}

	// Ground truth: v2 compiled cold, no state involved.
	want, err := c.CompileSource(ctx, v2)
	if err != nil {
		t.Fatalf("Compile (v2 cold): %v", err)
	}

	// v2 compiled against v1's stale state must produce identical output.
	var stderr bytes.Buffer
	got, err := c.CompileSource(ctx, v2, WithStateDir(stateDir), WithStderr(&stderr))
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

// TestCompileStateDirWithInputFS covers WithStateDir on a multi-file fs.FS
// with the main source in a subdirectory: the state files are named after the
// main source's basename and overlaid next to it, without touching the
// caller's filesystem.
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
	pdf1, err := c.Compile(ctx, fsys, "src/paper.tex", WithStateDir(stateDir), WithStderr(&stderr1))
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
	pdf2, err := c.Compile(ctx, fsys, "src/paper.tex", WithStateDir(stateDir), WithStderr(&stderr2))
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
	t.Logf("Got expected error: %v", err)
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
