package tecgonic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// itarEntry is one file in a synthetic itar bundle.
type itarEntry struct {
	name string
	data []byte
	gzip bool // whether to individually gzip the entry (as itar does for most files)
}

// buildITAR builds a tar archive whose entries are individually gzipped when
// requested, mimicking the Tectonic "itar" format.
func buildITAR(entries []itarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		payload := e.data
		if e.gzip {
			var gz bytes.Buffer
			w := gzip.NewWriter(&gz)
			_, _ = w.Write(e.data)
			_ = w.Close()
			payload = gz.Bytes()
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(err)
		}
		if _, err := tw.Write(payload); err != nil {
			panic(err)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

// syntheticBundle returns an itar with the sentinel file, a bit of metadata, and
// enough filler entries to clear the minimum-file-count check.
func syntheticBundle() []byte {
	entries := []itarEntry{
		{name: "SVNREV", data: []byte("12345"), gzip: false},
		{name: sentinelFile, data: []byte("% latex format source\n"), gzip: true},
		{name: "SHA256SUM", data: []byte("deadbeef  bundle\n"), gzip: false},
	}
	for i := 0; i < minBundleFiles+5; i++ {
		entries = append(entries, itarEntry{
			name: fmt.Sprintf("filler-%04d.sty", i),
			data: []byte(fmt.Sprintf("content %d", i)),
			gzip: true,
		})
	}
	return entries2tar(entries)
}

func entries2tar(entries []itarEntry) []byte { return buildITAR(entries) }

func serveBytes(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
}

func TestPrepareBundleExtracts(t *testing.T) {
	body := syntheticBundle()
	srv := serveBytes(t, body)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}

	// Sentinel and metadata extracted, decompressed.
	got, err := os.ReadFile(filepath.Join(dest, sentinelFile))
	if err != nil {
		t.Fatalf("reading sentinel: %v", err)
	}
	if !bytes.Contains(got, []byte("latex format source")) {
		t.Errorf("sentinel not decompressed: %q", got)
	}
	// Manifest written.
	if _, err := os.Stat(filepath.Join(dest, manifestName)); err != nil {
		t.Errorf("manifest missing: %v", err)
	}
	// No staging dir left behind.
	parent := filepath.Dir(dest)
	leftovers, _ := filepath.Glob(filepath.Join(parent, ".tecgonic-staging-*"))
	if len(leftovers) != 0 {
		t.Errorf("staging dirs left behind: %v", leftovers)
	}
}

func TestPrepareBundleRejectsTruncatedStream(t *testing.T) {
	body := syntheticBundle()
	// Strip the two-zero-block end-of-archive marker written by tar.Writer.Close.
	// The stream now ends cleanly on a 512-byte boundary, which archive/tar
	// reports as EOF — indistinguishable from a complete read without the marker
	// check (audit C2). All entries are still intact, so the file-count and
	// sentinel checks would pass.
	truncated := body[:len(body)-2*512]
	srv := serveBytes(t, truncated)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL))
	if err == nil {
		t.Fatal("expected error for a truncated bundle stream, got nil")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error does not indicate truncation: %v", err)
	}
	// The bundle must not be marked complete, so a later run re-downloads.
	if _, statErr := os.Stat(filepath.Join(dest, manifestName)); statErr == nil {
		t.Error("truncated bundle was marked complete")
	}
}

func TestPrepareBundleRejectsMalformedDigest(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(syntheticBundle())
	}))
	defer srv.Close()

	for _, digest := range []string{"deadbeef", strings.Repeat("z", 64), "abc123"} {
		dest := filepath.Join(t.TempDir(), "bundle")
		err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL), WithExpectedSHA256(digest))
		if err == nil {
			t.Fatalf("expected error for malformed digest %q, got nil", digest)
		}
		if !strings.Contains(err.Error(), "digest") && !strings.Contains(err.Error(), "hex") {
			t.Errorf("digest %q: error does not mention the digest: %v", digest, err)
		}
	}
	if hits != 0 {
		t.Errorf("a malformed digest triggered %d download(s); it must fail before downloading", hits)
	}
}

func TestPrepareBundleSkipsWhenComplete(t *testing.T) {
	body := syntheticBundle()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("first PrepareBundle: %v", err)
	}
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("second PrepareBundle: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected 1 download, got %d (completion marker not honored)", hits)
	}
}

func TestPrepareBundleReDownloadsOnURLChange(t *testing.T) {
	body := syntheticBundle()
	hitsA, hitsB := 0, 0
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		_, _ = w.Write(body)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		_, _ = w.Write(body)
	}))
	defer srvB.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srvA.URL)); err != nil {
		t.Fatalf("PrepareBundle (A): %v", err)
	}
	// A different URL over an existing bundle must re-download, not silently keep
	// whatever is on disk (audit C3).
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srvB.URL)); err != nil {
		t.Fatalf("PrepareBundle (B): %v", err)
	}
	if hitsB != 1 {
		t.Errorf("changing the URL did not re-download: srvB hits = %d, want 1", hitsB)
	}
	info, err := ReadBundleInfo(dest)
	if err != nil {
		t.Fatalf("ReadBundleInfo: %v", err)
	}
	if info.URL != srvB.URL {
		t.Errorf("recorded URL = %q, want %q", info.URL, srvB.URL)
	}
}

func TestPrepareBundleEnforcesPinAgainstExistingBundle(t *testing.T) {
	body := syntheticBundle()
	srv := serveBytes(t, body)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}

	// A pin that disagrees with the bundle already on disk must not be silently
	// ignored: it re-downloads and the fresh stream fails verification (audit C3).
	wrong := strings.Repeat("a", 64)
	err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL), WithExpectedSHA256(wrong))
	if err == nil {
		t.Fatal("expected a mismatch error when pinning a digest the bundle does not match, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error does not report a digest mismatch: %v", err)
	}
}

func TestReadBundleInfo(t *testing.T) {
	body := syntheticBundle()
	srv := serveBytes(t, body)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}

	info, err := ReadBundleInfo(dest)
	if err != nil {
		t.Fatalf("ReadBundleInfo: %v", err)
	}
	if info.URL != srv.URL {
		t.Errorf("URL = %q, want %q", info.URL, srv.URL)
	}
	if info.FileCount < minBundleFiles {
		t.Errorf("FileCount = %d, want >= %d", info.FileCount, minBundleFiles)
	}
	if len(info.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want a 64-char hex digest", info.SHA256)
	}

	// No bundle → error.
	if _, err := ReadBundleInfo(t.TempDir()); err == nil {
		t.Error("expected error reading bundle info from an empty dir")
	}
}

func TestPrepareBundleForceReextractsClearingStale(t *testing.T) {
	body := syntheticBundle()
	srv := serveBytes(t, body)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}

	// Simulate a generated latex.fmt (derived state) and a stale file left from
	// an older bundle.
	fmtPath := filepath.Join(dest, "latex.fmt")
	if err := os.WriteFile(fmtPath, []byte("OLD FORMAT"), 0o644); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(dest, "removed-in-v34.sty")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL), WithForce()); err != nil {
		t.Fatalf("force PrepareBundle: %v", err)
	}

	// Stale bundle file and derived format must be gone after a forced re-extract.
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale file survived forced re-extraction")
	}
	if _, err := os.Stat(fmtPath); !os.IsNotExist(err) {
		t.Errorf("derived latex.fmt survived forced re-extraction (C5)")
	}
}

func TestPrepareBundleRejectsTooFewFiles(t *testing.T) {
	body := buildITAR([]itarEntry{
		{name: sentinelFile, data: []byte("x"), gzip: true},
		{name: "one.sty", data: []byte("x"), gzip: true},
	})
	srv := serveBytes(t, body)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL))
	if err == nil {
		t.Fatal("expected error for too-few files, got nil")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("destDir should not exist after a failed extraction")
	}
}

func TestPrepareBundleRejectsMissingSentinel(t *testing.T) {
	var entries []itarEntry
	for i := 0; i < minBundleFiles+5; i++ {
		entries = append(entries, itarEntry{name: fmt.Sprintf("f%04d.sty", i), data: []byte("x"), gzip: true})
	}
	srv := serveBytes(t, buildITAR(entries))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL))
	if err == nil {
		t.Fatal("expected error for missing sentinel, got nil")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("destDir should not exist after a failed extraction")
	}
}

func TestPrepareBundleRejectsDuplicateBasename(t *testing.T) {
	entries := []itarEntry{{name: sentinelFile, data: []byte("x"), gzip: true}}
	for i := 0; i < minBundleFiles+5; i++ {
		entries = append(entries, itarEntry{name: fmt.Sprintf("f%04d.sty", i), data: []byte("x"), gzip: true})
	}
	// Two different paths collapsing to the same basename.
	entries = append(entries,
		itarEntry{name: "dirA/collide.sty", data: []byte("A"), gzip: true},
		itarEntry{name: "dirB/collide.sty", data: []byte("B"), gzip: true},
	)
	srv := serveBytes(t, buildITAR(entries))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL))
	if err == nil {
		t.Fatal("expected error for duplicate basename, got nil")
	}
}

func TestPrepareBundleVerifiesSHA256(t *testing.T) {
	body := syntheticBundle()
	srv := serveBytes(t, body)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "bundle")

	// Wrong digest fails and leaves nothing behind.
	err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL), WithExpectedSHA256("00"+hex.EncodeToString(make([]byte, 31))))
	if err == nil {
		t.Fatal("expected SHA-256 mismatch error, got nil")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("destDir should not exist after a digest mismatch")
	}

	// Correct digest succeeds.
	sum := sha256.Sum256(body)
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL), WithExpectedSHA256(hex.EncodeToString(sum[:]))); err != nil {
		t.Fatalf("PrepareBundle with correct digest: %v", err)
	}
}

func TestPrepareBundleAdoptsLegacyDir(t *testing.T) {
	// A directory that looks like a pre-manifest complete bundle should be
	// adopted (manifest written) without a re-download.
	dest := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, sentinelFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < minBundleFiles+5; i++ {
		if err := os.WriteFile(filepath.Join(dest, fmt.Sprintf("f%04d.sty", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be hit for an adoptable legacy bundle")
	}))
	defer srv.Close()

	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, manifestName)); err != nil {
		t.Errorf("legacy bundle not adopted: manifest missing")
	}
}

func TestPrepareBundleTinyAndEmptyEntries(t *testing.T) {
	// Entries shorter than the 2-byte gzip magic must extract as raw content,
	// not trip the sniff.
	entries := []itarEntry{
		{name: sentinelFile, data: []byte("x"), gzip: true},
		{name: "one-byte", data: []byte("Z"), gzip: false},
		{name: "empty", data: []byte(""), gzip: false},
		{name: "empty-gz", data: []byte(""), gzip: true},
	}
	for i := 0; i < minBundleFiles; i++ {
		entries = append(entries, itarEntry{name: fmt.Sprintf("f%04d.sty", i), data: []byte("content"), gzip: true})
	}
	srv := serveBytes(t, buildITAR(entries))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "one-byte"))
	if err != nil || string(got) != "Z" {
		t.Errorf("one-byte entry = %q, %v; want \"Z\"", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "empty")); err != nil || len(got) != 0 {
		t.Errorf("empty entry = %q, %v; want empty", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "empty-gz")); err != nil || len(got) != 0 {
		t.Errorf("empty gzip entry = %q, %v; want empty", got, err)
	}
}

// TestPrepareBundleRawEntryWithGzipMagic guards the regression where a raw
// (non-gzipped) binary entry whose first bytes coincidentally match the gzip
// magic (0x1f 0x8b) — as a real .tfm in the bundle does — must extract verbatim
// rather than fail as an invalid gzip stream.
func TestPrepareBundleRawEntryWithGzipMagic(t *testing.T) {
	// 0x1f 0x8b magic followed by an invalid compression method byte: looks like
	// gzip at two bytes, is not a valid gzip header.
	raw := []byte{0x1f, 0x8b, 0x99, 0x01, 0x02, 0x03, 0x04}
	entries := []itarEntry{
		{name: sentinelFile, data: []byte("x"), gzip: true},
		{name: "font.tfm", data: raw, gzip: false},
	}
	for i := 0; i < minBundleFiles; i++ {
		entries = append(entries, itarEntry{name: fmt.Sprintf("f%04d.sty", i), data: []byte("content"), gzip: true})
	}
	srv := serveBytes(t, buildITAR(entries))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "font.tfm"))
	if err != nil {
		t.Fatalf("reading raw entry: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("raw gzip-magic entry = %v; want verbatim %v", got, raw)
	}
}

func TestPrepareBundleHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "bundle")
	if err := PrepareBundle(context.Background(), dest, WithBundleURL(srv.URL)); err == nil {
		t.Fatal("expected error on HTTP 404, got nil")
	}
}
