package snapshot

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/secret"
)

func testSet(t *testing.T) *Set {
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	dir := t.TempDir()
	return &Set{Dir: filepath.Join(dir, "set"), Private: &PrivateStore{Dir: filepath.Join(dir, "private"), Key: func() (secret.Key, error) { return k, nil }}}
}

func TestPublicAndPrivateRoundTrip(t *testing.T) {
	s := testSet(t)
	pub := &Record{ID: "pub", Status: 200, Body: Body{Storage: Public}}
	priv := &Record{ID: "priv", Status: 200, Body: Body{Storage: Private}}
	if err := s.Write(pub, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(priv, []byte(`{"email":"x@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pub", "priv"} {
		r, err := s.Record(id)
		if err != nil || r == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if _, err := s.Body(r); err != nil {
			t.Fatalf("%s body: %v", id, err)
		}
	}
	filepath.Walk(filepath.Dir(s.Dir), func(p string, info os.FileInfo, _ error) error {
		if !info.IsDir() {
			raw, _ := os.ReadFile(p)
			if strings.Contains(string(raw), "x@example.com") {
				t.Errorf("private value in plain text: %s", p)
			}
		}
		return nil
	})
}

func TestPrivateRecordHasRefNotHash(t *testing.T) {
	s := testSet(t)
	body := []byte(`{"email":"x@example.com"}`)
	r := &Record{ID: "p", Status: 200, Body: Body{Storage: Private}}
	if err := s.Write(r, body); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir, "responses", "p.json"))
	if strings.Contains(string(raw), Sum(body)) || r.Body.SHA256 != "" || r.Body.Ref == "" {
		t.Fatalf("record: %s", raw)
	}
	key, _ := s.Private.Key()
	if r.Body.Ref != key.Ref(body) || !s.Private.Has(r.Body.Ref) {
		t.Fatalf("ref %s", r.Body.Ref)
	}
	// An empty private body is a body, not a missing one.
	e := &Record{ID: "e", Status: 204, Body: Body{Storage: Private}}
	if err := s.Write(e, []byte{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Body(e); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty body: %q %v", got, err)
	}
}

func TestPrivateBodyProblems(t *testing.T) {
	s := testSet(t)
	a := &Record{ID: "a", Status: 200, Body: Body{Storage: Private}}
	b := &Record{ID: "b", Status: 200, Body: Body{Storage: Private}}
	if err := s.Write(a, []byte("aaa")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(b, []byte("bbb")); err != nil {
		t.Fatal(err)
	}
	pa, pb := s.Private.path(a.Body.Ref), s.Private.path(b.Body.Ref)
	sealed, _ := os.ReadFile(pa)

	// A body stored under another body's name does not decrypt.
	os.WriteFile(pb, sealed, 0o600)
	if _, err := s.Body(b); err == nil {
		t.Fatal("swapped file accepted")
	}
	// A changed byte is found.
	sealed[len(sealed)-1] ^= 1
	os.WriteFile(pa, sealed, 0o600)
	if _, err := s.Body(a); err == nil {
		t.Fatal("tampered file accepted")
	}
	// A missing body is an error, not an empty body.
	os.Remove(pa)
	if _, err := s.Body(a); !errors.Is(err, ErrPrivateMissing) {
		t.Fatalf("missing: %v", err)
	}
	// A record from before refs names no body this store can find.
	old := &Record{ID: "old", Status: 200, Body: Body{Storage: Private, SHA256: Sum([]byte("gone"))}}
	if err := s.Write(old, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Body(old); !errors.Is(err, ErrPlainHash) {
		t.Fatalf("plain hash: %v", err)
	}
}

func TestGetPlain(t *testing.T) {
	s := testSet(t)
	s.Private.Ext = ".enc"
	key, _ := s.Private.Key()
	body := []byte("old body")
	sealed, _ := key.Seal(body, []byte(Sum(body)))
	os.MkdirAll(s.Private.Dir, 0o700)
	os.WriteFile(filepath.Join(s.Private.Dir, Sum(body)+".enc"), sealed, 0o600)
	if got, err := s.Private.GetPlain(Sum(body)); err != nil || string(got) != "old body" {
		t.Fatalf("GetPlain: %q %v", got, err)
	}
	if got, err := s.Private.GetPlain(Sum([]byte("gone"))); err != nil || got != nil {
		t.Fatalf("missing: %q %v", got, err)
	}
}

// TestPrivateBodyOutsideSet: a private body goes into the store, never next
// to the records.
func TestPrivateBodyOutsideSet(t *testing.T) {
	s := testSet(t)
	r := &Record{ID: "p", Status: 200, Body: Body{Storage: Private}}
	if err := s.Write(r, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !s.Private.Has(r.Body.Ref) {
		t.Fatal("not in the store")
	}
	filepath.Walk(s.Dir, func(path string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && !strings.HasPrefix(path, filepath.Join(s.Dir, "responses")) {
			t.Errorf("%s next to the records", path)
		}
		return nil
	})
	recs, err := s.Records()
	if err != nil || len(recs) != 1 || recs[0].ID != "p" {
		t.Fatalf("Records: %v %v", recs, err)
	}
}

func TestRejectsPathInID(t *testing.T) {
	s := testSet(t)
	for _, id := range []string{"../x", "a/b", "", ".hidden"} {
		if err := s.Write(&Record{ID: id, Body: Body{Storage: Public}}, []byte("x")); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
}

// TestNamesFromRecordsAreChecked: records come from the repository, so a
// body name or request ID is checked before it becomes a path.
func TestNamesFromRecordsAreChecked(t *testing.T) {
	s := testSet(t)
	outside := filepath.Join(filepath.Dir(s.Dir), "outside")
	os.WriteFile(outside, []byte("secret"), 0o600)
	for _, r := range []*Record{
		{ID: "a", Body: Body{Storage: Public, SHA256: "../outside"}},
		{ID: "a", Body: Body{Storage: Public, SHA256: "../../" + strings.Repeat("0", 64)}},
		{ID: "a", Body: Body{Storage: Private, Ref: "../../outside"}},
	} {
		if _, err := s.Body(r); err == nil || !strings.Contains(err.Error(), "invalid body name") {
			t.Errorf("%+v: %v", r.Body, err)
		}
	}
	if s.Private.Has("../outside") {
		t.Error("Has accepted a path")
	}
	if _, err := s.Private.GetPlain("../outside"); err == nil {
		t.Error("GetPlain accepted a path")
	}
	for _, id := range []string{"../outside", "a/b", ".hidden", ""} {
		if _, err := s.Record(id); err == nil {
			t.Errorf("Record(%q) accepted", id)
		}
		if err := s.Write(&Record{ID: id, Body: Body{Storage: Public}}, []byte("x")); err == nil {
			t.Errorf("Write(%q) accepted", id)
		}
	}
}

// A set that caches records reads each one once.
func TestCacheRecords(t *testing.T) {
	s := &Set{Dir: t.TempDir()}
	if err := s.Write(&Record{ID: "a", Status: 200, Body: Body{Storage: Public}}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	s.CacheRecords()
	r1, err := s.Record("a")
	if err != nil || r1 == nil {
		t.Fatal(r1, err)
	}
	if missing, err := s.Record("b"); missing != nil || err != nil {
		t.Fatal(missing, err)
	}
	os.RemoveAll(filepath.Join(s.Dir, "responses"))
	if r2, err := s.Record("a"); r2 != r1 || err != nil {
		t.Fatalf("read again: %v %v", r2, err)
	}
	if _, err := s.Record("../a"); err == nil {
		t.Fatal("invalid id accepted")
	}
}

// TestPrivateWrongKey: a private body read with another key is refused, and
// so is one whose decrypted content does not match its name.
func TestPrivateWrongKey(t *testing.T) {
	key := func() secret.Key {
		k := make([]byte, secret.KeySize)
		rand.Read(k)
		return k
	}
	k1, k2 := key(), key()
	dir := t.TempDir()
	s := &Set{Dir: dir, Private: &PrivateStore{Dir: filepath.Join(dir, "private"), Key: func() (secret.Key, error) { return k1, nil }}}
	r := &Record{ID: "a", Status: 200, Body: Body{Storage: Private}}
	if err := s.Write(r, []byte("personal")); err != nil {
		t.Fatal(err)
	}
	if body, err := s.Body(r); err != nil || string(body) != "personal" {
		t.Fatalf("body %q %v", body, err)
	}
	other := &PrivateStore{Dir: s.Private.Dir, Key: func() (secret.Key, error) { return k2, nil }}
	if _, err := other.Get(r.Body.Ref); err == nil || !strings.Contains(err.Error(), "wrong key") {
		t.Fatalf("another key: %v", err)
	}
	// A file sealed for this name with this key, but of another content.
	sealed, err := k1.Seal([]byte("other content"), []byte(r.Body.Ref))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.Private.path(r.Body.Ref), sealed, 0o600)
	if _, err := s.Private.Get(r.Body.Ref); err == nil || !strings.Contains(err.Error(), "does not match its name") {
		t.Fatalf("content under another name: %v", err)
	}
}

// TestPutInPlaceHoldsBodyOnce: a captured private body is encrypted in its
// own storage, so storing it allocates far less than the body; the file is
// the same format Get reads, and an existing body is not stored again.
func TestPutInPlaceHoldsBodyOnce(t *testing.T) {
	s := testSet(t)
	const size = 8 << 20
	orig := make([]byte, size)
	for i := range orig {
		orig[i] = byte('a' + i%26)
	}
	alloc := func(put func([]byte) (string, error)) (string, uint64) {
		body := make([]byte, size, size+512) // as the HTTP client reads it
		copy(body, orig)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		ref, err := put(body)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		return ref, after.TotalAlloc - before.TotalAlloc
	}
	ref, inPlace := alloc(s.Private.PutInPlace)
	got, err := s.Private.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("the stored body differs")
	}
	if inPlace > size/8 {
		t.Errorf("PutInPlace allocated %d bytes for a %d-byte body", inPlace, size)
	}
	os.RemoveAll(s.Private.Dir) // so Put stores it again
	if _, copied := alloc(s.Private.Put); copied < size {
		t.Errorf("Put allocated %d bytes; the comparison needs it to copy", copied)
	}
}

// Two records with the same public body write it once.
func TestSamePublicBodyWrittenOnce(t *testing.T) {
	s := testSet(t)
	body := []byte(`{"error":"not found"}`)
	if err := s.Write(&Record{ID: "a", Body: Body{Storage: Public}}, body); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "bodies", Sum(body))
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	if err := s.Write(&Record{ID: "b", Body: Body{Storage: Public}}, body); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || !st.ModTime().Equal(old) {
		t.Fatalf("the body was written again: %v", err)
	}
	if got, err := s.Body(&Record{ID: "b", Body: Body{Storage: Public, SHA256: Sum(body)}}); err != nil || string(got) != string(body) {
		t.Fatalf("%q %v", got, err)
	}
}

// TestRemoveStale: what writes of a killed process left in the store is
// removed; a file being written now, a file of another name, and anything
// in the store's own folder (the last comparison of a running compare)
// stay.
func TestRemoveStale(t *testing.T) {
	store := t.TempDir()
	old := time.Now().Add(-time.Hour)
	files := map[string]bool{ // path → removed
		"private/.tmp-123":                   true,
		"captures/run/bodies/.tmp-456":       true,
		"captures/run/responses/a.json":      false,
		"private/.tmp-new":                   false,
		".tmp-last-compare":                  false,
		"captures/run/responses/.tmp-x.json": true,
	}
	for path := range files {
		full := filepath.Join(store, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if path != "private/.tmp-new" {
			os.Chtimes(full, old, old)
		}
	}
	RemoveStale(store, time.Now().Add(-time.Minute))
	for path, removed := range files {
		if _, err := os.Stat(filepath.Join(store, path)); os.IsNotExist(err) != removed {
			t.Errorf("%s: removed %v, want %v", path, os.IsNotExist(err), removed)
		}
	}
}
