package tecgonic

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "latex.fmt")

	if err := writeFileAtomic(path, []byte("format data"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "format data" {
		t.Fatalf("content = %q, %v; want \"format data\"", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v, want 0644", info.Mode().Perm())
	}

	// Overwrite is atomic and leaves no temp files behind.
	if err := writeFileAtomic(path, []byte("newer data"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "newer data" {
		t.Errorf("content = %q; want \"newer data\"", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".latex.fmt.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// TestGenerateFormatUsesDefaultDir covers C9: an empty bundleDir falls back to
// the compiler's default. The pre-existing latex.fmt makes GenerateFormat a
// no-op, so this needs no real bundle.
func TestGenerateFormatUsesDefaultDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "latex.fmt"), []byte("fmt"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := New(ctx, WithDefaultBundleDir(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	// Empty bundleDir must resolve to the default and find the existing format.
	if err := c.GenerateFormat(ctx, ""); err != nil {
		t.Errorf("GenerateFormat(\"\") with default dir set: %v", err)
	}
}

func TestGenerateFormatNoDir(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	if err := c.GenerateFormat(ctx, ""); err == nil {
		t.Fatal("expected error when no bundle dir is available, got nil")
	}
}
