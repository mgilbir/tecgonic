package tecgonic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
)

const DefaultBundleURL = "https://relay.fullyjustified.net/default_bundle_v33.tar"

type prepareBundleConfig struct {
	progress io.Writer
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

// progressReader wraps an io.Reader and periodically reports bytes read.
type progressReader struct {
	r     io.Reader
	total int64 // from Content-Length, 0 if unknown
	read  atomic.Int64
	w     io.Writer
	last  int64 // last reported byte count
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		cur := pr.read.Add(int64(n))
		// Report every 10 MB
		if cur-pr.last >= 10*1024*1024 {
			pr.last = cur
			mb := cur / (1024 * 1024)
			if pr.total > 0 {
				totalMB := pr.total / (1024 * 1024)
				pct := cur * 100 / pr.total
				_, _ = fmt.Fprintf(pr.w, "  Downloading: %d / %d MB (%d%%)\n", mb, totalMB, pct)
			} else {
				_, _ = fmt.Fprintf(pr.w, "  Downloading: %d MB\n", mb)
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
// If destDir already contains SHA256SUM and force is false, the download is skipped.
// After extraction, call Compiler.GenerateFormat to generate the latex.fmt format file.
func PrepareBundle(ctx context.Context, destDir, bundleURL string, force bool, opts ...PrepareBundleOption) error {
	var cfg prepareBundleConfig
	for _, o := range opts {
		o(&cfg)
	}

	if bundleURL == "" {
		bundleURL = DefaultBundleURL
	}

	// Check if bundle already extracted (SHA256SUM is always present in the bundle tar)
	if !force {
		if _, err := os.Stat(filepath.Join(destDir, "SHA256SUM")); err == nil {
			return nil
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("tecgonic: creating bundle dir: %w", err)
	}

	// Download the bundle
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bundleURL, nil)
	if err != nil {
		return fmt.Errorf("tecgonic: creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("tecgonic: downloading bundle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tecgonic: downloading bundle: HTTP %d", resp.StatusCode)
	}

	// Wrap body with progress reader if progress reporting is enabled
	var body io.Reader = resp.Body
	if cfg.progress != nil {
		body = &progressReader{
			r:     resp.Body,
			total: resp.ContentLength,
			w:     cfg.progress,
		}
	}

	// Extract the tar archive
	tr := tar.NewReader(body)
	files := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tecgonic: reading tar entry: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(header.Name)
		destPath := filepath.Join(destDir, name)

		// Read the full entry into memory so we can attempt gzip decompression
		entryData, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("tecgonic: reading entry %s: %w", name, err)
		}

		// Try gzip decompression; fall back to raw content for metadata entries
		var reader io.Reader
		gr, gzErr := gzip.NewReader(bytes.NewReader(entryData))
		if gzErr == nil {
			reader = gr
		} else {
			reader = bytes.NewReader(entryData)
		}

		if err := writeFile(destPath, reader); err != nil {
			if gr != nil {
				_ = gr.Close()
			}
			return fmt.Errorf("tecgonic: writing %s: %w", name, err)
		}
		if gr != nil {
			_ = gr.Close()
		}

		files++
		if cfg.progress != nil && files%10000 == 0 {
			_, _ = fmt.Fprintf(cfg.progress, "  Extracted %d files\n", files)
		}
	}

	if cfg.progress != nil {
		_, _ = fmt.Fprintf(cfg.progress, "  Extracted %d files (done)\n", files)
	}

	// Validate that extraction produced files (check for a common TeX file)
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return fmt.Errorf("tecgonic: reading bundle dir: %w", err)
	}
	if len(entries) < 100 {
		return fmt.Errorf("tecgonic: bundle extraction incomplete: only %d files extracted", len(entries))
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
