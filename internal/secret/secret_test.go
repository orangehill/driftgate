package secret

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testKey(t *testing.T) Key {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal([]byte("secret body"), []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret body")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	pt, err := k.Open(sealed, []byte("abc"))
	if err != nil || string(pt) != "secret body" {
		t.Fatalf("Open: %q %v", pt, err)
	}
	if _, err := k.Open(sealed, []byte("other")); err == nil {
		t.Fatal("wrong aad accepted")
	}
	if _, err := testKey(t).Open(sealed, []byte("abc")); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestLoadFromEnv(t *testing.T) {
	k := testKey(t)
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(k))
	got, err := Load("p", Source{}, "")
	if err != nil || !bytes.Equal(got, k) {
		t.Fatalf("Load: %v", err)
	}
	t.Setenv(EnvKey, "c2hvcnQ=")
	if _, err := Load("p", Source{}, ""); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestRef(t *testing.T) {
	k, other := testKey(t), testKey(t)
	body := []byte(`{"email":"a@b.c"}`)
	ref := k.Ref(body)
	if len(ref) != 64 || ref != k.Ref(append([]byte{}, body...)) {
		t.Fatalf("Ref is not a stable hex HMAC: %q", ref)
	}
	if ref == other.Ref(body) {
		t.Fatal("two keys give the same ref")
	}
	if ref == k.Ref([]byte(`{"email":"a@b.d"}`)) {
		t.Fatal("two bodies give the same ref")
	}
	plain := sha256.Sum256(body)
	if ref == hex.EncodeToString(plain[:]) {
		t.Fatal("ref is the plain SHA-256")
	}
	// The ref key is derived, not the encryption key itself.
	mac := hmac.New(sha256.New, k)
	mac.Write(body)
	if ref == hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("ref uses the encryption key directly")
	}
}

func TestOpenDetectsTampering(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal([]byte("secret body"), []byte("ref"))
	if err != nil {
		t.Fatal(err)
	}
	for i := len(magic); i < len(sealed); i++ {
		bad := append([]byte{}, sealed...)
		bad[i] ^= 1
		if _, err := k.Open(bad, []byte("ref")); err == nil {
			t.Fatalf("byte %d flipped and accepted", i)
		}
	}
	if _, err := k.Open(sealed[:len(sealed)-1], []byte("ref")); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
}

// TestEnvironHidesCopiesOfTheKey: a variable holding the project key is
// left out of a command's environment whatever its name, once the key is
// loaded or a hidden variable holds it.
func TestEnvironHidesCopiesOfTheKey(t *testing.T) {
	k := testKey(t)
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(k))
	t.Setenv("DG_TEST_COPY", base64.StdEncoding.EncodeToString(k))
	t.Setenv("DG_TEST_URL", base64.RawURLEncoding.EncodeToString(k))
	t.Setenv("DG_TEST_HEX", hex.EncodeToString(k))
	t.Setenv("DG_TEST_OTHER", "keep me")
	loaded := testKey(t) // a key from 1Password or the Keychain: not in a hidden variable
	t.Setenv("DG_TEST_LOADED", base64.StdEncoding.EncodeToString(loaded)+"\n")
	if _, err := decode(base64.StdEncoding.EncodeToString(loaded)); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(Environ(), "\n")
	for _, name := range []string{EnvKey, "DG_TEST_COPY", "DG_TEST_URL", "DG_TEST_HEX", "DG_TEST_LOADED"} {
		if strings.Contains(env, name+"=") {
			t.Errorf("%s reaches commands", name)
		}
	}
	if !strings.Contains(env, "DG_TEST_OTHER=keep me") {
		t.Error("an unrelated variable is left out")
	}
}

// TestOpenInPlace: decrypting into the ciphertext's own memory gives what
// Open gives, without a second buffer, and refuses the same files: a
// flipped or missing byte, another aad, another key.
func TestOpenInPlace(t *testing.T) {
	k := testKey(t)
	for _, body := range [][]byte{{}, []byte("secret body"), bytes.Repeat([]byte("x"), 1<<20)} {
		sealed, err := k.Seal(body, []byte("ref"))
		if err != nil {
			t.Fatal(err)
		}
		open := func(k Key, b []byte, aad string) ([]byte, error) {
			return k.OpenInPlace(append([]byte{}, b...), []byte(aad))
		}
		want, err := k.Open(sealed, []byte("ref"))
		if err != nil {
			t.Fatal(err)
		}
		buf := append([]byte{}, sealed...)
		got, err := k.OpenInPlace(buf, []byte("ref"))
		if err != nil || !bytes.Equal(got, want) || !bytes.Equal(got, body) {
			t.Fatalf("%d bytes: %v", len(body), err)
		}
		if len(got) > 0 && &got[0] != &buf[len(magic)+12] {
			t.Fatal("the plaintext is not in the ciphertext's memory")
		}
		for _, i := range []int{len(magic), len(magic) + 5, len(magic) + 12, len(sealed) / 2, len(sealed) - 1} {
			bad := append([]byte{}, sealed...)
			bad[i] ^= 1
			if _, err := open(k, bad, "ref"); err == nil {
				t.Fatalf("%d bytes: byte %d flipped and accepted", len(body), i)
			}
		}
		if _, err := open(k, sealed[:len(sealed)-1], "ref"); err == nil {
			t.Fatal("truncated ciphertext accepted")
		}
		if _, err := open(k, sealed, "other"); err == nil {
			t.Fatal("wrong aad accepted")
		}
		if _, err := open(testKey(t), sealed, "ref"); err == nil {
			t.Fatal("wrong key accepted")
		}
	}
}

func TestSealInPlace(t *testing.T) {
	k := testKey(t)
	for _, size := range []int{0, 1, 1000, 1 << 20} {
		orig := make([]byte, size)
		rand.Read(orig)
		for _, room := range []int{0, 16, 512} {
			buf := make([]byte, size, size+room)
			copy(buf, orig)
			header, ct, err := k.SealInPlace(buf, []byte("ref"))
			if err != nil {
				t.Fatal(err)
			}
			if inPlace := size > 0 && &ct[0] == &buf[0]; inPlace != (room >= 16 && size > 0) {
				t.Errorf("size %d, room %d: in place %v", size, room, inPlace)
			}
			sealed := append(append([]byte{}, header...), ct...)
			got, err := k.Open(sealed, []byte("ref"))
			if err != nil || !bytes.Equal(got, orig) {
				t.Fatalf("size %d, room %d: %v", size, room, err)
			}
			if _, err := k.Open(sealed, []byte("other")); err == nil {
				t.Fatal("opened with another aad")
			}
			sealed[len(sealed)-1] ^= 1
			if _, err := k.Open(sealed, []byte("ref")); err == nil {
				t.Fatal("opened a changed file")
			}
		}
	}
}

func TestKeyFile(t *testing.T) {
	t.Setenv(EnvKey, "")
	path := filepath.Join(t.TempDir(), ".driftgate", "key")
	if _, err := Load("p", Source{}, path); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no file: %v", err)
	}
	if err := CreateFile(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	k, err := Load("p", Source{}, path)
	if err != nil || base64.StdEncoding.EncodeToString(k)+"\n" != string(raw) {
		t.Fatalf("Load: %v (%q)", err, raw)
	}
	if err := CreateFile(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second key: %v", err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, raw) {
		t.Fatal("the key was replaced")
	}
	// The environment comes first, as it does before the Keychain.
	other := testKey(t)
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(other))
	if got, _ := Load("p", Source{}, path); !bytes.Equal(got, other) {
		t.Fatal("DRIFTGATE_KEY did not take precedence over the key file")
	}
	t.Setenv(EnvKey, "")
	if runtime.GOOS != "windows" {
		os.Chmod(path, 0o644)
		if _, err := Load("p", Source{}, path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("key file open to others: %v", err)
		}
	}
}
