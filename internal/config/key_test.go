package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

// localWithBody is a local project outside git whose store holds one
// private body sealed with the key returned; its key file is gone.
func localWithBody(t *testing.T) (*Project, secret.Key, string) {
	t.Helper()
	t.Setenv(EnvStore, "")
	t.Setenv(secret.EnvKey, "")
	p, err := New(Config{Version: Version, Project: "shop", Mode: Freeze, Level: "values", Baselines: BaselinesLocal}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k := secret.Key(bytes.Repeat([]byte{3}, secret.KeySize))
	store, err := p.Private()
	if err != nil {
		t.Fatal(err)
	}
	store.Key = func() (secret.Key, error) { return k, nil }
	ref, err := store.Put([]byte(`{"email":"a@b.c"}`))
	if err != nil {
		t.Fatal(err)
	}
	return p, k, ref
}

// TestNewKeyWouldOrphanBodies: with private bodies in the store and their
// key not here, key create refuses (a new key cannot read them) and the
// missing-key error says to put the key back.
func TestNewKeyWouldOrphanBodies(t *testing.T) {
	p, k, _ := localWithBody(t)
	if _, err := p.CreateKey(); err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "sealed with a key that is not here") {
		t.Fatalf("key create: %v", err)
	}
	if exists(p.Layout.Key) {
		t.Fatal("a key was made")
	}
	if _, err := p.LoadKey(); !errors.Is(err, secret.ErrNoKey) || !strings.Contains(err.Error(), "put that key back") {
		t.Fatalf("no key: %v", err)
	}
	// The key is in DRIFTGATE_KEY: keep that one in the file.
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	if _, err := p.CreateKey(); err == nil || !strings.Contains(err.Error(), "printf") {
		t.Fatalf("key create with the key in DRIFTGATE_KEY: %v", err)
	}
}

// TestKeyConflict: DRIFTGATE_KEY overrides .driftgate/key; when they
// differ a body the variable's key cannot open says so.
func TestKeyConflict(t *testing.T) {
	p, k, ref := localWithBody(t)
	if err := secret.WriteFile(p.Layout.Key, k); err != nil {
		t.Fatal(err)
	}
	if c := p.KeyConflict(); c != "" || p.KeySource() != filepath.Join(".driftgate", "key") {
		t.Fatalf("key file only: %q %s", c, p.KeySource())
	}
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, secret.KeySize)))
	if c := p.KeyConflict(); !strings.Contains(c, "DRIFTGATE_KEY is set and differs from "+filepath.Join(".driftgate", "key")) {
		t.Fatalf("conflict: %q", c)
	}
	store, err := p.Private()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ref); err == nil || !strings.Contains(err.Error(), "DRIFTGATE_KEY is set and differs") {
		t.Fatalf("body under the other key: %v", err)
	}
	os.Remove(p.Layout.Key)
}
