package tecgonic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const DefaultBundleURL = "https://relay.fullyjustified.net/default_bundle_v33.tar"

// manifestName is the completion marker written atomically as the last step of
// a successful extraction. Its presence — not the presence of any bundle file —
// is what marks a bundle directory as complete and usable.
const manifestName = "tecgonic-bundle.json"

// sentinelFile must be present in every valid bundle. latex.ltx (the LaTeX
// format source) ships in the Tectonic default bundle and is required for
// format generation, so its absence means extraction did not produce a usable
// bundle.
const sentinelFile = "latex.ltx"

// minBundleFiles is a floor on the number of files a real extraction writes.
// The Tectonic default bundle holds ~134k files; a run that produced fewer than
// this many did not extract a real bundle.
const minBundleFiles = 100

// bundleManifest records what was extracted so that a later run can tell a
// complete bundle from a partial one, and so callers can see the bundle's
// identity.
type bundleManifest struct {
	BundleURL string `json:"bundle_url"`
	FileCount int    `json:"file_count"`
	// SHA256 is the hex-encoded digest of the raw downloaded tar stream.
	SHA256 string `json:"sha256"`
}

type prepareBundleConfig struct {
	progress     io.Writer
	client       *http.Client
	expectSHA256 string
}

// PrepareBundleOption configures a PrepareBundle call.
type PrepareBundleOption func(*prepareBundleConfig)

// WithProgress enables progress reporting to the given writer.
// Download progress (bytes/percentage) and extraction progress (file count) are reported.
func WithProgress(w io.Writer) PrepareBundleOption {
	return func(c *prepareBundleConfig) {
		c.progress = w
	}
}

// WithHTTPClient sets the HTTP client used to download the bundle. This lets
// callers impose a timeout (see net/http.Client.Timeout) or route the request
// through a custom transport. When unset, a client with no timeout is used, so
// the download is bounded only by the context passed to PrepareBundle.
func WithHTTPClient(client *http.Client) PrepareBundleOption {
	return func(c *prepareBundleConfig) {
		c.client = client
	}
}

// WithExpectedSHA256 verifies that the SHA-256 of the downloaded tar stream
// matches the given hex-encoded digest before the bundle is committed to
// destDir. A mismatch fails PrepareBundle and leaves destDir untouched. Use
// this to pin a known-good bundle and detect corruption or a substituted
// mirror. When unset, no cryptographic verification is performed.
func WithExpectedSHA256(hexDigest string) PrepareBundleOption {
	return func(c *prepareBundleConfig) {
		c.expectSHA256 = hexDigest
	}
}

// countingHashReader tracks the number of bytes read and the running SHA-256 of
// the stream, and reports download progress.
type countingHashReader struct {
	r     io.Reader
	h     hash.Hash
	total int64 // from Content-Length, 0 if unknown
	read  int64
	w     io.Writer // progress sink, nil to disable
	last  int64     // last reported byte count
}

func (cr *countingHashReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		_, _ = cr.h.Write(p[:n])
		cr.read += int64(n)
		if cr.w != nil && cr.read-cr.last >= 10*1024*1024 {
			cr.last = cr.read
			mb := cr.read / (1024 * 1024)
			if cr.total > 0 {
				totalMB := cr.total / (1024 * 1024)
				pct := cr.read * 100 / cr.total
				_, _ = fmt.Fprintf(cr.w, "  Downloading: %d / %d MB (%d%%)\n", mb, totalMB, pct)
			} else {
				_, _ = fmt.Fprintf(cr.w, "  Downloading: %d MB\n", mb)
			}
		}
	}
	return n, err
}

// PrepareBundle downloads and extracts a Tectonic TeX Live bundle to destDir.
//
// The bundle is an "itar" format: a tar archive where most entries are individually
// gzip-compressed. Metadata entries (like SVNREV) may not be compressed.
// Files are extracted to a flat directory structure.
//
// Extraction is atomic: the bundle is unpacked into a temporary staging
// directory alongside destDir and swapped into place only after it is fully
// written and validated, so an interrupted or failed run never leaves a partial
// bundle that a later call mistakes for a complete one. A forced re-extraction
// replaces destDir wholesale, clearing any stale files (including a previously
// generated latex.fmt, which is derived state keyed to the bundle).
//
// If destDir already holds a complete bundle and force is false, the download is
// skipped. After extraction, call Compiler.GenerateFormat to generate the
// latex.fmt format file.
func PrepareBundle(ctx context.Context, destDir, bundleURL string, force bool, opts ...PrepareBundleOption) error {
	var cfg prepareBundleConfig
	for _, o := range opts {
		o(&cfg)
	}

	if bundleURL == "" {
		bundleURL = DefaultBundleURL
	}
	client := cfg.client
	if client == nil {
		client = http.DefaultClient
	}

	// Fast path: a complete bundle is already present.
	if !force && bundleComplete(destDir) {
		return nil
	}

	// Extract into a staging directory on the same filesystem as destDir so the
	// final swap is an atomic rename.
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("tecgonic: creating bundle parent dir: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".tecgonic-staging-*")
	if err != nil {
		return fmt.Errorf("tecgonic: creating staging dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()

	manifest, err := downloadAndExtract(ctx, client, bundleURL, staging, &cfg)
	if err != nil {
		return err
	}

	if err := validateExtraction(staging, manifest.FileCount); err != nil {
		return err
	}

	// Write the completion marker atomically as the final step inside staging,
	// before the swap, so destDir is marked complete only once everything else
	// is in place.
	if err := writeManifest(staging, manifest); err != nil {
		return err
	}

	if err := swapIntoPlace(staging, destDir); err != nil {
		return err
	}
	committed = true
	return nil
}

// bundleComplete reports whether destDir holds a complete, usable bundle.
//
// Completion is authoritatively marked by the manifest file. A bundle extracted
// by an older version of tecgonic has no manifest; if such a directory still
// looks complete (sentinel file present and enough files), it is adopted by
// writing a manifest, so an existing good bundle is not needlessly re-downloaded.
func bundleComplete(destDir string) bool {
	if _, err := os.Stat(filepath.Join(destDir, manifestName)); err == nil {
		return true
	}
	// Legacy adoption: a pre-manifest bundle that still looks complete.
	if _, err := os.Stat(filepath.Join(destDir, sentinelFile)); err != nil {
		return false
	}
	entries, err := os.ReadDir(destDir)
	if err != nil || len(entries) < minBundleFiles {
		return false
	}
	// Best-effort: record a manifest so future checks use the strong marker.
	// If this fails (e.g. read-only dir), the bundle is still usable.
	_ = writeManifest(destDir, bundleManifest{FileCount: len(entries)})
	return true
}

// downloadAndExtract streams the bundle from bundleURL into destDir and returns
// a manifest describing what was written. It verifies the stream digest against
// cfg.expectSHA256 when set.
func downloadAndExtract(ctx context.Context, client *http.Client, bundleURL, destDir string, cfg *prepareBundleConfig) (bundleManifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bundleURL, nil)
	if err != nil {
		return bundleManifest{}, fmt.Errorf("tecgonic: creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return bundleManifest{}, fmt.Errorf("tecgonic: downloading bundle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return bundleManifest{}, fmt.Errorf("tecgonic: downloading bundle: HTTP %d", resp.StatusCode)
	}

	reader := &countingHashReader{
		r:     resp.Body,
		h:     sha256.New(),
		total: resp.ContentLength,
		w:     cfg.progress,
	}

	files, err := extractTar(reader, destDir, cfg.progress)
	if err != nil {
		return bundleManifest{}, err
	}

	// Consume any bytes trailing the tar's end-of-archive marker so the digest
	// covers the entire response body.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return bundleManifest{}, fmt.Errorf("tecgonic: reading bundle stream: %w", err)
	}

	digest := hex.EncodeToString(reader.h.Sum(nil))
	if cfg.expectSHA256 != "" && !strings.EqualFold(digest, cfg.expectSHA256) {
		return bundleManifest{}, fmt.Errorf("tecgonic: bundle SHA-256 mismatch: got %s, want %s", digest, cfg.expectSHA256)
	}

	return bundleManifest{BundleURL: bundleURL, FileCount: files, SHA256: digest}, nil
}

// extractTar unpacks the itar stream into destDir (flat) and returns the number
// of regular files written. Duplicate basenames are rejected: the default
// Tectonic bundle is intentionally flat with unique names, so a collision means
// a custom bundle whose structure the flattening would silently corrupt.
func extractTar(r io.Reader, destDir string, progress io.Writer) (int, error) {
	tr := tar.NewReader(r)
	// Reused across entries so a ~134k-file bundle does not allocate a fresh
	// entry buffer (and gzip reader) per entry.
	var buf bytes.Buffer
	var gr *gzip.Reader
	seen := make(map[string]struct{})
	files := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, fmt.Errorf("tecgonic: reading tar entry: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(header.Name)
		if _, dup := seen[name]; dup {
			return files, fmt.Errorf("tecgonic: duplicate bundle entry %q: flat extraction cannot represent a structured bundle", name)
		}
		seen[name] = struct{}{}
		destPath := filepath.Join(destDir, name)

		// Buffer the entry into a reused buffer, then decide how to write it.
		// Most entries are individually gzipped; metadata entries (SVNREV,
		// SHA256SUM) are stored raw, as can be binary files (e.g. a .tfm) whose
		// first bytes happen to match the gzip magic — so decompression is
		// attempted and the raw bytes are used when the gzip header is invalid.
		// The reused buffer avoids a fresh allocation for each of ~134k entries.
		buf.Reset()
		if _, err := io.Copy(&buf, tr); err != nil {
			return files, fmt.Errorf("tecgonic: reading entry %s: %w", name, err)
		}
		src := decompressor(buf.Bytes(), &gr)
		if err := writeFile(destPath, src); err != nil {
			return files, fmt.Errorf("tecgonic: writing %s: %w", name, err)
		}

		files++
		if progress != nil && files%10000 == 0 {
			_, _ = fmt.Fprintf(progress, "  Extracted %d files\n", files)
		}
	}

	if progress != nil {
		_, _ = fmt.Fprintf(progress, "  Extracted %d files (done)\n", files)
	}
	return files, nil
}

// decompressor returns a reader over data, transparently gunzipping it when it
// is a valid gzip stream. grp holds a reused *gzip.Reader (created on first use)
// to avoid a per-entry allocation. When data is not a valid gzip stream —
// including raw entries whose bytes coincidentally begin with the gzip magic —
// the raw bytes are returned unchanged.
func decompressor(data []byte, grp **gzip.Reader) io.Reader {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return bytes.NewReader(data)
	}
	if *grp == nil {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return bytes.NewReader(data)
		}
		*grp = gr
		return gr
	}
	if err := (*grp).Reset(bytes.NewReader(data)); err != nil {
		return bytes.NewReader(data)
	}
	return *grp
}

// validateExtraction checks that the staging directory holds a plausible
// bundle: a sentinel file that every bundle must contain, and enough files to
// rule out an empty or truncated archive. Only files written this run are
// counted (the staging dir starts empty), so pre-existing unrelated files in
// destDir cannot mask an incomplete extraction.
func validateExtraction(stagingDir string, fileCount int) error {
	if fileCount < minBundleFiles {
		return fmt.Errorf("tecgonic: bundle extraction incomplete: only %d files extracted", fileCount)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, sentinelFile)); err != nil {
		return fmt.Errorf("tecgonic: bundle extraction invalid: sentinel %q missing", sentinelFile)
	}
	return nil
}

func writeManifest(dir string, m bundleManifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("tecgonic: encoding manifest: %w", err)
	}
	tmp := filepath.Join(dir, manifestName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("tecgonic: writing manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, manifestName)); err != nil {
		return fmt.Errorf("tecgonic: committing manifest: %w", err)
	}
	return nil
}

// swapIntoPlace atomically replaces destDir with staging, removing any stale
// bundle already present. The two renames leave a brief window in which destDir
// does not exist; a concurrent reader sees either the old bundle or the new one,
// never a mixture.
func swapIntoPlace(staging, destDir string) error {
	backup := ""
	if _, err := os.Stat(destDir); err == nil {
		backup = destDir + ".old-" + filepath.Base(staging)
		if err := os.Rename(destDir, backup); err != nil {
			return fmt.Errorf("tecgonic: moving old bundle aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("tecgonic: inspecting bundle dir: %w", err)
	}

	if err := os.Rename(staging, destDir); err != nil {
		if backup != "" {
			_ = os.Rename(backup, destDir) // restore old bundle
		}
		return fmt.Errorf("tecgonic: installing bundle: %w", err)
	}

	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by an atomic rename, so a concurrent reader sees either the previous
// contents or the complete new contents, never a partial write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp) // clean up on failure; no-op once renamed
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = "" // committed
	return nil
}
