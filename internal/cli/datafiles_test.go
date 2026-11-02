package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
)

// TestStoragePlaceholders: data.files gives the whole folder placeholders
// under the same names, keeps file columns as they are, and refuses a
// target inside the source or one that is not empty.
func TestStoragePlaceholders(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "storage", "avatars", "pera.txt"), "Pera Perić, pera@example.com")
	write(t, filepath.Join(root, "storage", "thumbs", "x.csv"), "name;email\nPera;pera@example.com\n")
	p := &config.Project{Root: root}

	if f, err := storagePlaceholders(p); f != nil || err != nil {
		t.Fatalf("without data.files: %v %v", f, err)
	}
	p.Data.Files = config.DataFiles{Source: "storage", Target: "storage/copy"}
	if _, err := storagePlaceholders(p); err == nil {
		t.Fatal("a target inside the source was accepted")
	}
	write(t, filepath.Join(root, "busy", "a"), "x")
	p.Data.Files.Target = "busy"
	if _, err := storagePlaceholders(p); err == nil {
		t.Fatal("a target that is not empty was accepted")
	}

	p.Data.Files.Target = "placeholders"
	f, err := storagePlaceholders(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Placeholder(context.Background(), data.FileValue{Path: "avatars/pera.txt"}); got != "avatars/pera.txt" {
		t.Errorf("a file column changed: %s", got)
	}
	var out bytes.Buffer
	if err := writePlaceholders(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"avatars/pera.txt", "thumbs/x.csv"} {
		raw, err := os.ReadFile(filepath.Join(root, "placeholders", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(raw), "pera") || strings.Contains(string(raw), "Pera") {
			t.Errorf("%s holds original content: %q", name, raw)
		}
	}
	if !strings.Contains(out.String(), "2 placeholders") {
		t.Errorf("summary: %s", out.String())
	}
}
