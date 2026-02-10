// Command simple demonstrates the full tecgonic workflow:
// download a TeX bundle, generate the format file, and compile LaTeX to PDF.
//
// Usage:
//
//	go run . [-o output.pdf] [-bundle-dir ~/.cache/tecgonic/bundle]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/mgilbir/tecgonic"
)

var texSource = []byte(`\documentclass{article}
\begin{document}
Hello, World!
\end{document}
`)

func main() {
	defaultBundleDir := ""
	if cacheDir, err := os.UserCacheDir(); err == nil {
		defaultBundleDir = cacheDir + "/tecgonic/bundle"
	}

	output := flag.String("o", "output.pdf", "output PDF path")
	bundleDir := flag.String("bundle-dir", defaultBundleDir, "path to TeX bundle directory")
	flag.Parse()

	if *bundleDir == "" {
		fmt.Fprintln(os.Stderr, "error: cannot determine bundle directory; set -bundle-dir")
		os.Exit(1)
	}

	if err := run(context.Background(), *bundleDir, *output); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, bundleDir, output string) error {
	// Step 1: Download and extract the TeX bundle (~800 MB, skipped if already present).
	fmt.Fprintln(os.Stderr, "Preparing bundle...")
	if err := tecgonic.PrepareBundle(ctx, bundleDir, "", false, tecgonic.WithProgress(os.Stderr)); err != nil {
		return fmt.Errorf("preparing bundle: %w", err)
	}

	// Step 2: Create the compiler (pre-compiles the WASM module).
	fmt.Fprintln(os.Stderr, "Initializing compiler...")
	compiler, err := tecgonic.New(ctx, tecgonic.WithDefaultBundleDir(bundleDir))
	if err != nil {
		return fmt.Errorf("creating compiler: %w", err)
	}
	defer compiler.Close(ctx)

	// Step 3: Generate the LaTeX format file (skipped if latex.fmt already exists).
	fmt.Fprintln(os.Stderr, "Generating format...")
	if err := compiler.GenerateFormat(ctx, bundleDir); err != nil {
		return fmt.Errorf("generating format: %w", err)
	}

	// Step 4: Compile LaTeX to PDF.
	fmt.Fprintln(os.Stderr, "Compiling LaTeX...")
	pdf, err := compiler.Compile(ctx, texSource, tecgonic.WithStderr(os.Stderr))
	if err != nil {
		return fmt.Errorf("compiling: %w", err)
	}

	// Step 5: Write the PDF.
	if err := os.WriteFile(output, pdf, 0o644); err != nil {
		return fmt.Errorf("writing PDF: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Wrote %s (%d bytes)\n", output, len(pdf))
	return nil
}
