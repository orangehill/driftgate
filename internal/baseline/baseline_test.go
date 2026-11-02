package baseline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newBaseline makes a baseline with two records, a body and two acceptances.
func newBaseline(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "meta.json"), `{"version":1}`+"\n")
	write(t, filepath.Join(dir, "responses", "a.json"), `{"version":1,"id":"a"}`+"\n")
	write(t, filepath.Join(dir, "responses", "b.json"), `{"version":1,"id":"b"}`+"\n")
	write(t, filepath.Join(dir, "bodies", "x"), "body")
	for _, reason := range []string{"first", "second"} {
		if err := Append(dir, Acceptance{At: "2026-10-29T10:00:00Z", By: "T <t@t>", Reason: reason, Requests: []string{"a"}, Capture: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(dir); err != nil {
		t.Fatal(err)
	}
	if res, err := Verify(dir); err != nil || !res.OK() || res.Files != 4 || res.Acceptances != 2 {
		t.Fatalf("fresh baseline: %v %+v", err, res)
	}
	return dir
}

func problems(t *testing.T, dir string) string {
	t.Helper()
	res, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(res.Problems, "\n")
}

func TestVerify(t *testing.T) {
	cases := []struct {
		name   string
		change func(dir string)
		want   string
	}{
		{"hand edit", func(dir string) {
			write(t, filepath.Join(dir, "responses", "a.json"), `{"version":1,"id":"a","status":200}`+"\n")
		}, "responses/a.json: changed"},
		{"extra file", func(dir string) {
			write(t, filepath.Join(dir, "responses", "c.json"), `{}`)
		}, "responses/c.json: not in the manifest"},
		{"deleted file", func(dir string) {
			os.Remove(filepath.Join(dir, "bodies", "x"))
		}, "bodies/x: missing"},
		{"edited log line", func(dir string) {
			path := filepath.Join(dir, LogFile)
			raw, _ := os.ReadFile(path)
			write(t, path, strings.Replace(string(raw), `"first"`, `"other"`, 1))
		}, "line 2 does not follow line 1"},
		{"edited last log line", func(dir string) {
			path := filepath.Join(dir, LogFile)
			raw, _ := os.ReadFile(path)
			write(t, path, strings.Replace(string(raw), `"second"`, `"other"`, 1))
		}, "line 2 (the last) differs from the manifest"},
		{"removed log line", func(dir string) {
			path := filepath.Join(dir, LogFile)
			raw, _ := os.ReadFile(path)
			write(t, path, strings.SplitAfter(string(raw), "\n")[0])
		}, "1 lines, the manifest records 2"},
		{"removed first log line", func(dir string) {
			path := filepath.Join(dir, LogFile)
			raw, _ := os.ReadFile(path)
			write(t, path, strings.SplitAfter(string(raw), "\n")[1])
		}, "line 1 refers to a line before it"},
		{"removed log", func(dir string) {
			os.Remove(filepath.Join(dir, LogFile))
		}, "0 lines, the manifest records 2"},
		{"log without final newline", func(dir string) {
			path := filepath.Join(dir, LogFile)
			raw, _ := os.ReadFile(path)
			write(t, path, strings.TrimSuffix(string(raw), "\n"))
		}, "no newline"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newBaseline(t)
			c.change(dir)
			if got := problems(t, dir); !strings.Contains(got, c.want) {
				t.Fatalf("want %q, got:\n%s", c.want, got)
			}
		})
	}
}

func TestVerifyIgnoresFinderMetadata(t *testing.T) {
	dir := newBaseline(t)
	write(t, filepath.Join(dir, "responses", ".DS_Store"), "x")
	if got := problems(t, dir); got != "" {
		t.Fatal(got)
	}
}

func TestNoManifest(t *testing.T) {
	dir := newBaseline(t)
	os.Remove(filepath.Join(dir, ManifestFile))
	if _, err := Verify(dir); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("got %v", err)
	}
}

func TestLogChain(t *testing.T) {
	dir := newBaseline(t)
	log, err := Log(dir)
	if err != nil || len(log) != 2 {
		t.Fatalf("%v %v", err, log)
	}
	if log[0].Prev != "" || log[1].Prev == "" || log[1].Version != Version {
		t.Fatalf("chain: %+v", log)
	}
}

func TestSetDigest(t *testing.T) {
	dir := newBaseline(t)
	d1, _ := SetDigest(dir)
	write(t, filepath.Join(dir, "timings.json"), "{}") // not part of the content
	d2, _ := SetDigest(dir)
	write(t, filepath.Join(dir, "responses", "a.json"), "{}")
	d3, _ := SetDigest(dir)
	if d1 != d2 || d2 == d3 {
		t.Fatalf("digests %s %s %s", d1, d2, d3)
	}
}

// TestSymlinks: a baseline that is a link, or holds one, is refused.
func TestSymlinks(t *testing.T) {
	dir := newBaseline(t)
	outside := filepath.Join(t.TempDir(), "x.json")
	write(t, outside, "{}")
	if err := os.Symlink(outside, filepath.Join(dir, "responses", "link.json")); err != nil {
		t.Skip(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link inside: %v", err)
	}
	os.Remove(filepath.Join(dir, "responses", "link.json"))
	link := filepath.Join(t.TempDir(), "local")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if _, err := Verify(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a linked baseline: %v", err)
	}
}

// TestVerifyLineEndings: a baseline git checked out with CRLF line endings
// (core.autocrlf) says so, file by file; the acceptance log and the
// manifest themselves are read line by line and stay valid. A file changed
// otherwise is only changed.
func TestVerifyLineEndings(t *testing.T) {
	dir := newBaseline(t)
	for _, name := range []string{"meta.json", "responses/a.json", LogFile, ManifestFile} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		raw, _ := os.ReadFile(path)
		write(t, path, strings.ReplaceAll(string(raw), "\n", "\r\n"))
	}
	res, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "meta.json: changed (only its line endings: CRLF instead of LF)\nresponses/a.json: changed (only its line endings: CRLF instead of LF)"
	if got := strings.Join(res.Problems, "\n"); got != want || !res.LineEndings {
		t.Fatalf("problems:\n%s\nline endings %v", got, res.LineEndings)
	}
	write(t, filepath.Join(dir, "responses", "a.json"), `{"version":1,"id":"a","status":200}`+"\r\n")
	if res, _ := Verify(dir); !strings.Contains(strings.Join(res.Problems, "\n"), "responses/a.json: changed") {
		t.Fatalf("problems %v", res.Problems)
	}
}
