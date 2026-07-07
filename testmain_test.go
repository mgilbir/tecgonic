package tecgonic

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// testBundleDir is a ready-to-use bundle directory for the functional tests.
// It points at TECGONIC_BUNDLE_DIR when set (a full extracted bundle), otherwise
// at the small committed testdata bundle extracted by TestMain. Empty only if
// neither is available.
var testBundleDir string

// TestMain provisions a bundle directory so the functional tests run on a fresh
// clone with no setup: it extracts testdata/minibundle.tar.gz — a minimal but
// real bundle (article.cls, Latin Modern, latex.fmt) sufficient for the compile
// tests — into a temp dir, unless TECGONIC_BUNDLE_DIR already points at one.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if dir := os.Getenv("TECGONIC_BUNDLE_DIR"); dir != "" {
		testBundleDir = dir
		return m.Run()
	}

	dir, err := extractTestBundle("testdata/minibundle.tar.gz")
	if err != nil {
		// Leave testBundleDir empty; functional tests skip with a clear reason.
		fmt.Fprintf(os.Stderr, "tecgonic: test bundle unavailable, functional tests will skip: %v\n", err)
		return m.Run()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	testBundleDir = dir
	return m.Run()
}

func extractTestBundle(archivePath string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer func() { _ = gr.Close() }()

	dir, err := os.MkdirTemp("", "tecgonic-testbundle-*")
	if err != nil {
		return "", err
	}

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dest := filepath.Join(dir, filepath.Base(hdr.Name))
		out, err := os.Create(dest)
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // trusted committed archive
			_ = out.Close()
			_ = os.RemoveAll(dir)
			return "", err
		}
		if err := out.Close(); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}
