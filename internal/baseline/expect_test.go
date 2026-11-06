package baseline

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/snapshot"
)

func TestExpectationsLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	if exps, refs, err := Expectations(dir); err != nil || len(refs) != 0 || len(exps) != 0 {
		t.Fatalf("no folder: %v %v %v", exps, refs, err)
	}
	add := func(l ExpectLine) {
		t.Helper()
		if err := AppendExpectation(dir, l); err != nil {
			t.Fatal(err)
		}
	}
	in := func() string {
		t.Helper()
		exps, _, err := Expectations(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range exps {
			got = append(got, e.Ref+" "+e.ID+" "+strings.Join(e.Drifts, "+"))
		}
		return strings.Join(got, ", ")
	}
	add(ExpectLine{Op: OpExpect, Ref: "T-1", Reason: "ids are numbers", Requests: []Expected{{ID: "a", Drifts: []string{"sha256:1"}}, {ID: "b", Drifts: []string{"sha256:2"}}}})
	add(ExpectLine{Op: OpExpect, Ref: "T-2", Reason: "new header", Requests: []Expected{{ID: "a", Drifts: []string{"sha256:3"}}}})
	add(ExpectLine{Op: OpClose, Ref: "T-1", Requests: []Expected{{ID: "b", Drifts: []string{"sha256:2"}}}})
	if got := in(); got != "T-1 a sha256:1, T-2 a sha256:3" {
		t.Fatalf("in force: %s", got)
	}
	// Expecting a request of a ref again replaces its earlier expectation.
	add(ExpectLine{Op: OpExpect, Ref: "T-1", Reason: "both", Requests: []Expected{{ID: "a", Drifts: []string{"sha256:1", "sha256:4"}}}})
	if got := in(); got != "T-1 a sha256:1+sha256:4, T-2 a sha256:3" {
		t.Fatalf("superseded: %s", got)
	}
	add(ExpectLine{Op: OpDrop, Ref: "T-1"})
	if got := in(); got != "T-2 a sha256:3" {
		t.Fatalf("after drop: %s", got)
	}
	for _, bad := range []ExpectLine{{Op: OpExpect, Ref: "T-3"}, {Op: OpDrop}, {Op: "other", Ref: "x"}, {Op: OpExpect, Ref: "x", Requests: []Expected{{ID: "a"}}}} {
		if err := AppendExpectation(dir, bad); err == nil {
			t.Errorf("appended %+v", bad)
		}
	}
	// One file per ref: two tickets never change the same file.
	files, _ := filepath.Glob(filepath.Join(ExpectDir(dir), "*"))
	if len(files) != 2 || filepath.Base(files[0]) != "T-1.jsonl" || filepath.Base(files[1]) != "T-2.jsonl" {
		t.Fatalf("files: %v", files)
	}

	// Every change by hand shows: an edited line, a removed one, lines
	// added or removed at the end, the seal edited, an empty file, a file
	// of another baseline or renamed to another ref.
	path := filepath.Join(ExpectDir(dir), "T-1.jsonl")
	good, _ := os.ReadFile(path)
	lines := bytes.SplitAfter(good, []byte("\n"))
	lines = lines[:len(lines)-1]
	if len(lines) != 5 {
		t.Fatalf("lines: %d", len(lines))
	}
	join := func(ls ...[]byte) string { return string(bytes.Join(ls, nil)) }
	other := filepath.Join(t.TempDir(), "release")
	for name, tc := range map[string]struct{ file, content string }{
		"edited first line":  {path, strings.Replace(string(good), "sha256:1", "sha256:9", 1)},
		"edited last line":   {path, join(lines[0], lines[1], lines[2], bytes.Replace(lines[3], []byte(`"op":"drop"`), []byte(`"op":"drop" `), 1), lines[4])},
		"removed line":       {path, join(lines[0], lines[2], lines[3], lines[4])},
		"seal removed":       {path, join(lines[0], lines[1], lines[2], lines[3])},
		"last line and seal": {path, join(lines[0], lines[1], lines[2], lines[4])},
		"line after seal":    {path, join(lines...) + string(lines[0])},
		"no newline":         {path, strings.TrimSuffix(string(good), "\n")},
		"empty":              {path, ""},
		"empty line":         {path, join(lines[0], []byte("\n"), lines[1], lines[2], lines[3], lines[4])},
		"renamed":            {filepath.Join(ExpectDir(dir), "T-9.jsonl"), string(good)},
		"another file":       {filepath.Join(ExpectDir(dir), "notes.txt"), "x"},
		"another baseline":   {filepath.Join(ExpectDir(other), "T-1.jsonl"), string(good)},
	} {
		os.WriteFile(path, good, 0o644)
		target := dir
		if strings.HasPrefix(tc.file, ExpectDir(other)) {
			target = other
		}
		if tc.file != path && filepath.Dir(tc.file) == ExpectDir(dir) {
			os.Remove(path)
		}
		os.MkdirAll(filepath.Dir(tc.file), 0o755)
		if err := os.WriteFile(tc.file, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := Expectations(target)
		var bad *ExpectProblems
		if !errors.As(err, &bad) || len(bad.Problems) == 0 || !strings.Contains(err.Error(), "changed outside driftgate expect") {
			t.Errorf("%s: %v", name, err)
		}
		if err := AppendExpectation(target, ExpectLine{Op: OpDrop, Ref: "T-2"}); err == nil {
			t.Errorf("%s: appended to a changed folder", name)
		}
		if tc.file != path {
			os.Remove(tc.file)
		}
	}
	os.WriteFile(path, good, 0o644)
	if _, _, err := Expectations(dir); err != nil {
		t.Fatal(err)
	}
}

// TestRefFile: a ref that is a plain file name names its file; any other
// is written safely, with a hash that keeps two refs apart.
func TestRefFile(t *testing.T) {
	for ref, want := range map[string]string{
		"API-7":      "API-7.jsonl",
		"api_7.1":    "api_7.1.jsonl",
		"a/b":        "a_b~",
		"a_b":        "a_b.jsonl",
		"../x":       "_._x~",
		".hidden":    "_hidden~",
		"con":        "con~",
		"https://x/": "https___x_~",
	} {
		if got := RefFile(ref, false); !strings.HasPrefix(got, want) || !strings.HasSuffix(got, ".jsonl") || strings.ContainsAny(got, `/\`) {
			t.Errorf("%q: %s", ref, got)
		}
	}
	if RefFile("a/b", false) == RefFile("a:b", false) || RefFile("API-7", true) == RefFile("API-7", false) {
		t.Error("two refs share a file")
	}
	// A ref that differs only in letter case gets the hashed name where the
	// plain one is taken (macOS and Windows file names).
	dir := filepath.Join(t.TempDir(), "local")
	for _, ref := range []string{"api-7", "API-7"} {
		if err := AppendExpectation(dir, ExpectLine{Op: OpExpect, Ref: ref, Requests: []Expected{{ID: "a", Drifts: []string{"x"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if exps, refs, err := Expectations(dir); err != nil || len(refs) != 2 || len(exps) != 2 {
		t.Fatalf("%v %v %v", exps, refs, err)
	}
}

// TestExpectationsScale: reading is linear in the number of
// expectations.
func TestExpectationsScale(t *testing.T) {
	read := func(n int) time.Duration {
		dir := filepath.Join(t.TempDir(), "local")
		for ref := 0; ref < 10; ref++ {
			var reqs []Expected
			for i := 0; i < n/10; i++ {
				reqs = append(reqs, Expected{ID: fmt.Sprintf("r-%d", i), Drifts: []string{fmt.Sprintf("sha256:%d-%d", ref, i)}})
			}
			if err := AppendExpectation(dir, ExpectLine{Op: OpExpect, Ref: fmt.Sprintf("T-%d", ref), Requests: reqs}); err != nil {
				t.Fatal(err)
			}
		}
		best := time.Hour
		for range 3 {
			start := time.Now()
			if exps, _, err := Expectations(dir); err != nil || len(exps) != n {
				t.Fatalf("%v %d", err, len(exps))
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	small, large := read(1000), read(8000)
	if large > 20*small {
		t.Errorf("8,000 expectations take %v, 1,000 take %v: not linear", large, small)
	}
}

// TestRefFileLong: a ref longer than 60 characters is cut and hashed
// with 8 bytes, so two long refs with one beginning get two files.
func TestRefFileLong(t *testing.T) {
	long := strings.Repeat("x", 61)
	if name := RefFile(long, false); !strings.Contains(name, "~") || len(name) != 60+1+16+len(".jsonl") {
		t.Errorf("long ref: %s", name)
	}
	if name := RefFile(strings.Repeat("x", 60), false); name != strings.Repeat("x", 60)+".jsonl" {
		t.Errorf("60 characters: %s", name)
	}
	dir := filepath.Join(t.TempDir(), "local")
	for _, ref := range []string{long + "a", long + "b"} {
		if err := AppendExpectation(dir, ExpectLine{Op: OpExpect, Ref: ref, Requests: []Expected{{ID: "a", Drifts: []string{"x"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, refs, err := Expectations(dir); err != nil || len(refs) != 2 {
		t.Fatalf("%v %v", refs, err)
	}
}

// TestImportSealOnly: a file with only its seal is refused, imported or
// in the baseline, and nothing is written.
func TestImportSealOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	seal, err := encodeLine(ExpectLine{Version: Version, Op: opSeal, Baseline: "local", Ref: "T-1", Lines: 0})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "T-1.jsonl")
	if err := os.WriteFile(file, append(seal, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	var problems *ExpectProblems
	if _, _, err := ImportExpectations(dir, file); !errors.As(err, &problems) || !strings.Contains(err.Error(), "only its seal") {
		t.Fatalf("import: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(ExpectDir(dir), "*")); len(files) != 0 {
		t.Fatalf("written: %v", files)
	}
	if err := os.MkdirAll(ExpectDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ExpectDir(dir), "T-1.jsonl"), append(seal, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Expectations(dir); !errors.As(err, &problems) {
		t.Fatalf("in the baseline: %v", err)
	}
}

// TestExpectationsLeftover: a file a killed write left in the folder is
// no ref's file: the expectations read without it, and the next write
// removes it once it is older than a minute.
func TestExpectationsLeftover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	if err := AppendExpectation(dir, ExpectLine{Op: OpExpect, Ref: "T-1", Reason: "r", Requests: []Expected{{ID: "a", Drifts: []string{"sha256:1"}}}}); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(ExpectDir(dir), snapshot.TempPrefix+"123")
	if err := os.WriteFile(left, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if exps, _, err := Expectations(dir); err != nil || len(exps) != 1 {
		t.Fatalf("with a leftover: %v %v", exps, err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(left, old, old); err != nil {
		t.Fatal(err)
	}
	if err := AppendExpectation(dir, ExpectLine{Op: OpExpect, Ref: "T-1", Reason: "r", Requests: []Expected{{ID: "b", Drifts: []string{"sha256:2"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("leftover kept: %v", err)
	}
}
