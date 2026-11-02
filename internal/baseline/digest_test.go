package baseline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/snapshot"
)

// setDigestAll is SetDigest as it was: every file of the set hashed, the
// records and meta.json kept.
func setDigestAll(t *testing.T, dir string) string {
	t.Helper()
	sums, err := Sums(dir)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range sums {
		if k == "meta.json" || strings.HasPrefix(k, "responses/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", sums[k], k)
	}
	return snapshot.Sum([]byte(b.String()))
}

// TestSetDigestReadsRecordsOnly: the digest is the one of every file
// filtered to the records and meta.json, and no body is read.
func TestSetDigestReadsRecordsOnly(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "meta.json"), `{"version":1,"tag":"t"}`+"\n")
	write(t, filepath.Join(dir, "timings.json"), `{"a":3}`)
	write(t, filepath.Join(dir, "responses", ".DS_Store"), "finder")
	for i := range 50 {
		write(t, filepath.Join(dir, "responses", fmt.Sprintf("r%02d.json", i)), fmt.Sprintf(`{"version":1,"id":"r%02d"}`, i))
		write(t, filepath.Join(dir, "bodies", fmt.Sprintf("b%02d", i)), strings.Repeat("x", i))
	}
	want := setDigestAll(t, dir)
	got, err := SetDigest(dir)
	if err != nil || got != want {
		t.Fatalf("digest %s %v, want %s", got, err, want)
	}
	// A body that cannot be read changes nothing: bodies are not read.
	if err := os.Chmod(filepath.Join(dir, "bodies", "b07"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := SetDigest(dir); err != nil || got != want {
		t.Fatalf("with an unreadable body: %s %v", got, err)
	}
	os.Chmod(filepath.Join(dir, "bodies", "b07"), 0o644)
	write(t, filepath.Join(dir, "responses", "r03.json"), `{"version":1,"id":"r03","status":500}`)
	if got, err := SetDigest(dir); err != nil || got == want || got != setDigestAll(t, dir) {
		t.Fatalf("after a changed record: %s %v", got, err)
	}
	empty := t.TempDir()
	if got, err := SetDigest(empty); err != nil || got != setDigestAll(t, empty) {
		t.Fatalf("empty set: %s %v", got, err)
	}
	if err := os.Symlink(filepath.Join(dir, "meta.json"), filepath.Join(empty, "meta.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := SetDigest(empty); err == nil {
		t.Fatal("a linked meta.json was accepted")
	}
}

// TestWriteFrom: a manifest from the hashes taken while copying is the one
// Write computes.
func TestWriteFrom(t *testing.T) {
	dir := newBaseline(t)
	want, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "bodies", ".DS_Store"), "finder")
	written := map[string]string{"bodies/.DS_Store": snapshot.Sum([]byte("finder")), ManifestFile: "x", LogFile: "y"}
	for _, rel := range []string{"meta.json", "responses/a.json", "responses/b.json", "bodies/x"} {
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		written[rel] = snapshot.Sum(raw)
	}
	if err := WriteFrom(dir, written); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ManifestFile)); string(got) != string(want) {
		t.Fatalf("manifest\n%s\nwant\n%s", got, want)
	}
}

// TestStamp: the stamp changes with a new file or a written one, and not
// when nothing is written.
func TestStamp(t *testing.T) {
	dir := newBaseline(t)
	stamp := func() string {
		s, err := Stamp(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := stamp()
	if stamp() != first {
		t.Fatal("stamp changed without a write")
	}
	write(t, filepath.Join(dir, "responses", "c.json"), "{}")
	if stamp() == first {
		t.Fatal("a new file kept the stamp")
	}
	second := stamp()
	write(t, filepath.Join(dir, "bodies", "x"), "bodyy")
	if stamp() == second {
		t.Fatal("a changed body kept the stamp")
	}
}
