package files

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)], _ = os.ReadFile(p)
		}
		return nil
	})
	return out
}

func TestCopyTree(t *testing.T) {
	var img bytes.Buffer
	png.Encode(&img, pattern(40, 30))
	src := filepath.Join(t.TempDir(), "storage")
	writeTree(t, src, map[string][]byte{
		"app/public/avatars/1/jovana.png":   img.Bytes(),
		"app/public/avatars/2/photo.png":    img.Bytes(),
		"app/public/docs/cv.pdf":            samplePDF(false),
		"app/private/exports/users.csv":     []byte("name,email\n" + marker + "\n"),
		"app/private/exports/huge.bin":      bytes.Repeat([]byte{1}, 4096),
		"app/private/secret/unreadable.txt": []byte(marker),
		"app/locked/inside.txt":             []byte(marker),
		"logs/laravel.log":                  []byte("[2026-10-29] " + marker + "\n"),
		"empty.txt":                         nil,
	})
	os.Symlink(filepath.Join(src, "app/public"), filepath.Join(src, "public-link"))
	os.Symlink("/etc/passwd", filepath.Join(src, "app/public/docs/passwd"))
	os.Chmod(filepath.Join(src, "app/private/secret/unreadable.txt"), 0)
	os.Chmod(filepath.Join(src, "app/locked"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "app/locked"), 0o755) })
	if _, err := os.ReadFile(filepath.Join(src, "app/private/secret/unreadable.txt")); err == nil {
		t.Skip("running as a user that reads everything")
	}

	opt := Options{MaxSize: 4000}
	dst := filepath.Join(t.TempDir(), "copy")
	s, err := CopyTree(context.Background(), src, dst, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := &Summary{
		Files:    6,
		Dirs:     11,
		Kinds:    map[string]int{PNG: 2, PDF: 1, CSV: 1, Text: 1, Empty: 1},
		Symlinks: []string{"app/public/docs/passwd", "public-link"},
		Skipped: []Skipped{
			{"app/locked", "unreadable"},
			{"app/private/exports/huge.bin", "too large"},
			{"app/private/secret/unreadable.txt", "unreadable"},
		},
		Bytes: s.Bytes,
	}
	if !reflect.DeepEqual(s, want) {
		g, _ := json.MarshalIndent(s, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("summary\n%s\nwant\n%s", g, w)
	}
	got := readTree(t, dst)
	var names []string
	for rel, data := range got {
		names = append(names, rel)
		if bytes.Contains(data, []byte(marker)) || bytes.Contains(data, img.Bytes()[40:80]) {
			t.Fatalf("%s: original content in the copy", rel)
		}
	}
	for _, rel := range []string{"app/public/avatars/1/jovana.png", "app/public/docs/cv.pdf", "logs/laravel.log", "empty.txt"} {
		if _, ok := got[rel]; !ok {
			t.Fatalf("%s missing from the copy: %v", rel, names)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "public-link")); err == nil {
		t.Fatal("symlink copied")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(got["app/public/avatars/1/jovana.png"]))
	if err != nil || cfg.Width != 40 || cfg.Height != 30 {
		t.Fatalf("png %+v %v", cfg, err)
	}
	if summary, _ := json.Marshal(s); bytes.Contains(summary, []byte("Jovana")) {
		t.Fatal("content in the summary")
	}

	// The same tree gives the same copy.
	dst2 := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTree(context.Background(), src, dst2, opt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, readTree(t, dst2)) {
		t.Fatal("copies differ")
	}
}

func TestCopyTreeRefuses(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string][]byte{"a.txt": []byte("x")})
	for name, dst := range map[string]string{
		"inside":    filepath.Join(src, "copy"),
		"same":      src,
		"not empty": src + "/..",
	} {
		_, err := CopyTree(context.Background(), src, dst, Options{})
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "copy")); err == nil {
		t.Fatal("target created inside the source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CopyTree(ctx, src, filepath.Join(t.TempDir(), "c"), Options{}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancelled: %v", err)
	}
}
