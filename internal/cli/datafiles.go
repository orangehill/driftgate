package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/data/files"
)

func init() { filePlaceholders = storagePlaceholders }

// storageFiles serves file columns: the whole folder of uploaded files gets
// placeholders under the same names (also files no column names, such as
// generated thumbnails), so a file column keeps its value.
type storageFiles struct{ source, target string }

func (storageFiles) Placeholder(ctx context.Context, f data.FileValue) (string, error) {
	return f.Path, nil
}

// storagePlaceholders is nil without data.files: a file rule then stops the
// run before anything is written.
func storagePlaceholders(p *config.Project) (data.FilePlaceholder, error) {
	if p.Data.Files.Source == "" {
		return nil, nil
	}
	src, dst := p.Data.Files.Source, p.Data.Files.Target
	if !filepath.IsAbs(src) {
		src = filepath.Join(p.Root, src)
	}
	if !filepath.IsAbs(dst) {
		dst = filepath.Join(p.Root, dst)
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("data.files.source %s is not a folder", src)
	}
	// The placeholders must not land in the originals, nor the originals be
	// copied into themselves.
	rel, err := filepath.Rel(src, dst)
	if err != nil || rel == "." || !strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("data.files.target %s must be outside data.files.source", dst)
	}
	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("data.files.target %s is not empty; remove it first", dst)
	}
	return storageFiles{source: src, target: dst}, nil
}

// writePlaceholders writes the placeholders of the whole folder, once the database
// output passed data verify. The summary holds counts and paths, never
// content.
func writePlaceholders(ctx context.Context, f data.FilePlaceholder, out io.Writer) error {
	s, ok := f.(storageFiles)
	if !ok {
		return nil
	}
	sum, err := files.CopyTree(ctx, s.source, s.target, files.Options{})
	if err != nil {
		return envErr(fmt.Errorf("file placeholders: %w", err))
	}
	fmt.Fprintf(out, "files: %d placeholders in %s (%d symbolic links, %d skipped)\n", sum.Files, s.target, len(sum.Symlinks), len(sum.Skipped))
	for _, sk := range sum.Skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", sk.Path, sk.Reason)
	}
	return nil
}
