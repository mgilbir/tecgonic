package tecgonic

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func bundleDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("TECGONIC_BUNDLE_DIR")
	if dir == "" {
		t.Skip("TECGONIC_BUNDLE_DIR not set")
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

	_, err = c.Compile(ctx, tex)
	if err == nil {
		t.Fatal("expected error when no bundle dir set, got nil")
	}

	t.Logf("Got expected error: %v", err)
}
