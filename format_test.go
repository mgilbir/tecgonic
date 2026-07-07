package tecgonic

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestIsStateFile(t *testing.T) {
	cases := map[string]bool{
		"input.aux":        true,
		"input.toc":        true,
		"input.out":        true,
		"input.bbl":        true,
		"input.nav":        true, // beamer
		"input.snm":        true, // beamer
		"input.idx":        true, // makeidx
		"input.run.xml":    true, // biber control file
		"input.pdf":        false,
		"input.log":        false,
		"input.xdv":        false,
		"input.synctex.gz": false,
		"input.tex":        false, // the source must never be seeded back
		"other.aux":        false, // only input.* intermediates
		"input":            false,
	}
	for name, want := range cases {
		if got := isStateFile(name, "input"); got != want {
			t.Errorf("isStateFile(%q, \"input\") = %v, want %v", name, got, want)
		}
	}
	// The predicate keys on the document's own basename, not a fixed "input".
	if !isStateFile("paper.aux", "paper") {
		t.Error(`isStateFile("paper.aux", "paper") = false, want true`)
	}
	if isStateFile("input.aux", "paper") {
		t.Error(`isStateFile("input.aux", "paper") = true, want false`)
	}
}

func TestWithMaxPassesInvalid(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx, WithDefaultBundleDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	tex := []byte(`\documentclass{article}\begin{document}Hi\end{document}`)
	for _, n := range []int{0, -1} {
		_, err := c.CompileSource(ctx, tex, WithMaxPasses(n))
		if err == nil {
			t.Errorf("WithMaxPasses(%d): expected error, got nil", n)
		}
	}
}

func TestBoundedBuffer(t *testing.T) {
	b := &boundedBuffer{max: 8}
	if _, err := b.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "abc" {
		t.Errorf("under cap: %q, want \"abc\"", got)
	}
	// Overflow keeps the most recent bytes and marks truncation.
	_, _ = b.Write([]byte("defghijkl")) // total "abcdefghijkl" -> keep last 8
	if !b.truncated {
		t.Error("expected truncated=true after overflow")
	}
	got := b.String()
	if want := "[earlier tectonic output truncated]\nefghijkl"; got != want {
		t.Errorf("after overflow: %q, want %q", got, want)
	}
}

// TestBoundedBufferHugeWrite checks that a single write far larger than the cap
// keeps only the tail and does not retain the whole write (audit C15).
func TestBoundedBufferHugeWrite(t *testing.T) {
	b := &boundedBuffer{max: 8}
	p := append(bytes.Repeat([]byte("."), 1000), []byte("12345678")...)
	if _, err := b.Write(p); err != nil {
		t.Fatal(err)
	}
	if !b.truncated {
		t.Error("expected truncated=true after an oversized write")
	}
	if got := string(b.buf); got != "12345678" {
		t.Errorf("buf = %q, want the last 8 bytes", got)
	}
	// The transient must be bounded: the buffer must not retain all 1008 bytes.
	if cap(b.buf) > b.max {
		t.Errorf("buf cap = %d, want <= %d", cap(b.buf), b.max)
	}
}

// TestWithMemoryLimitMiBClamps pins the page arithmetic: no panic-inducing page
// count above the wasm32 ceiling, and no uint32 wraparound at extreme values
// (audit C4).
func TestWithMemoryLimitMiBClamps(t *testing.T) {
	cases := []struct {
		mib   int
		pages uint32
	}{
		{-5, 0},
		{0, 0},
		{1, 16},
		{1024, 16384},
		{4096, maxWasmPages},
		{5000, maxWasmPages},    // previously panicked New (80000 > 65536)
		{1 << 28, maxWasmPages}, // previously wrapped uint32 to 0 (no limit)
		{1 << 40, maxWasmPages},
	}
	for _, tc := range cases {
		var cfg compilerConfig
		WithMemoryLimitMiB(tc.mib)(&cfg)
		if cfg.memoryLimitPages != tc.pages {
			t.Errorf("WithMemoryLimitMiB(%d) = %d pages, want %d", tc.mib, cfg.memoryLimitPages, tc.pages)
		}
	}
}

// TestNewLargeMemoryLimitNoPanic guards the C4 regression directly: an oversized
// limit must not panic inside New.
func TestNewLargeMemoryLimitNoPanic(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx, WithMemoryLimitMiB(5000))
	if err != nil {
		t.Fatalf("New with an oversized memory limit: %v", err)
	}
	_ = c.Close(ctx)
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

// TestGenerateFormatValidatesBundleDir covers audit C3: GenerateFormat must
// reject a nonexistent bundle dir up front with a config-shaped error (mirroring
// Compile), not run the engine against a phantom mount and return an *EngineError.
func TestGenerateFormatValidatesBundleDir(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()

	err = c.GenerateFormat(ctx, filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for a nonexistent bundle dir, got nil")
	}
	if !strings.Contains(err.Error(), "bundle directory") {
		t.Errorf("error does not mention the bundle directory: %v", err)
	}
	var eng *EngineError
	if errors.As(err, &eng) {
		t.Errorf("a typo'd bundle dir surfaced as *EngineError, want a plain config error: %v", err)
	}
}
