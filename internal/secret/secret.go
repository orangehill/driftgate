// Package secret holds the key that encrypts private response bodies and the
// encryption itself (AES-256-GCM). The key comes from the environment, the
// macOS Keychain or 1Password, or, for a project that keeps everything in
// its own folder (baselines: local), from a key file there that git ignores
// and only its owner can read.
package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// KeySize is the length of an AES-256 key.
const KeySize = 32

// EnvKey is the environment variable that may hold the base64 key.
const EnvKey = "DRIFTGATE_KEY"

const keychainService = "driftgate"

var magic = []byte("DGE1")

// hidden holds the names of the environment variables that hold a key
// (DRIFTGATE_KEY and the key.env of a project).
var hidden sync.Map

// hiddenValues holds the text forms of every key loaded or named so far
// (keyForms): a variable with one of them as its value is hidden too,
// whatever its name.
var hiddenValues sync.Map

// HideEnv keeps an environment variable that holds a key from the commands
// DriftGate runs.
func HideEnv(name string) {
	if name != "" {
		hidden.Store(name, true)
	}
}

// hideKey hides every variable whose value is the key in a usual text form.
func hideKey(k Key) {
	for _, f := range keyForms(k) {
		hiddenValues.Store(f, true)
	}
}

// keyForms are the texts a key is usually written as: base64 (standard and
// URL alphabets, with and without padding) and hex.
func keyForms(k Key) []string {
	return []string{
		base64.StdEncoding.EncodeToString(k), base64.RawStdEncoding.EncodeToString(k),
		base64.URLEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k),
		hex.EncodeToString(k), strings.ToUpper(hex.EncodeToString(k)),
	}
}

// Environ is the environment of a command DriftGate runs (hooks, adapters,
// token commands, git): its own, without the variables that hold a key. The
// commands come from the repository, which a pull request may change.
//
// A variable is left out by its name (DRIFTGATE_KEY, key.env) or by its
// value: the project key as loaded, or the key a hidden variable holds, in
// any of keyForms, so a copy of the key under another name (a CI secret
// exported twice) is left out too. This is defence in depth, not a
// boundary: a key in another form (in a file, inside a longer value) or a
// command that reads the key from where DriftGate got it (1Password, the
// Keychain) is not stopped.
func Environ() []string {
	env := os.Environ()
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if _, ok := hidden.Load(name); ok || name == EnvKey {
			if k, err := decode(strings.TrimSpace(value)); err == nil {
				hideKey(k)
			}
		}
	}
	var out []string
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if _, ok := hidden.Load(name); ok || name == EnvKey {
			continue
		}
		if _, ok := hiddenValues.Load(strings.TrimSpace(value)); ok {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ErrNoKey means no key source produced a key.
var ErrNoKey = errors.New("no encryption key: set " + EnvKey + ", run 'driftgate key create', or configure key.op")

// Source says where the key comes from; empty fields fall back to the
// environment and then the macOS Keychain.
type Source struct {
	Env string `yaml:"env,omitempty"`
	Op  string `yaml:"op,omitempty"`
}

// Key is an AES-256 key.
type Key []byte

// Load finds the key for a project. file is the key file of a project that
// keeps its key in its own folder; when it is set it takes the place of the
// Keychain.
func Load(project string, src Source, file string) (Key, error) {
	switch {
	case src.Op != "":
		cmd := exec.Command("op", "read", src.Op)
		cmd.Env = Environ()
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("op read %s: %w", src.Op, err)
		}
		return decode(strings.TrimSpace(string(out)))
	case src.Env != "":
		if v := os.Getenv(src.Env); v != "" {
			return decode(v)
		}
		return nil, ErrNoKey
	}
	if v := os.Getenv(EnvKey); v != "" {
		return decode(v)
	}
	if file != "" {
		return ReadFile(file)
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-a", project, "-w").Output()
		if err == nil {
			return decode(strings.TrimSpace(string(out)))
		}
	}
	return nil, ErrNoKey
}

// decode reads a base64 key; a key read is hidden from commands
// (Environ) under every name.
func decode(s string) (Key, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(k) != KeySize {
		return nil, fmt.Errorf("encryption key must be %d bytes, base64-encoded", KeySize)
	}
	hideKey(k)
	return Key(k), nil
}

// Create generates a key and stores it in the macOS Keychain. The key is
// passed to `security` on stdin, never as an argument visible in the process list.
func Create(project string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("the Keychain is only available on macOS; set %s to a base64 key of %d random bytes instead", EnvKey, KeySize)
	}
	if err := exec.Command("security", "find-generic-password", "-s", keychainService, "-a", project).Run(); err == nil {
		return fmt.Errorf("a key for %q already exists in the Keychain; delete it first if you really want a new one (old private bodies become unreadable)", project)
	}
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return err
	}
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -s %s -a %s -w %s\n", keychainService, quoteArg(project), base64.StdEncoding.EncodeToString(k)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security: %v: %s", err, out)
	}
	return nil
}

// ReadFile reads a key file: a base64 key that only its owner can read.
func ReadFile(path string) (Key, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if fi.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		return nil, fmt.Errorf("key file %s can be read by others (mode %v); chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1024))
	if err != nil {
		return nil, err
	}
	k, err := decode(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return k, nil
}

// CreateFile generates a key and writes it to path (base64, open to its
// owner only). An existing file is never replaced: private bodies encrypted
// with its key would become unreadable.
func CreateFile(path string) error {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return err
	}
	return WriteFile(path, k)
}

// WriteFile writes k to a new key file at path.
func WriteFile(path string, k Key) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("a key already exists in %s; delete it first if you really want a new one (old private bodies become unreadable)", path)
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func quoteArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Seal encrypts plaintext; aad binds the ciphertext to its name.
func (k Key) Seal(plaintext, aad []byte) ([]byte, error) {
	gcm, err := k.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte{}, magic...), nonce...)
	return gcm.Seal(out, nonce, plaintext, aad), nil
}

// SealInPlace is Seal that encrypts into plaintext's own storage, so a body
// is held once: plaintext is overwritten and must not be used afterwards.
// The header (format mark and nonce) comes back apart from the ciphertext;
// written one after the other they are what Seal returns. Without room for
// the tag in plaintext's capacity the ciphertext gets storage of its own.
func (k Key) SealInPlace(plaintext, aad []byte) (header, ciphertext []byte, err error) {
	gcm, err := k.gcm()
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	header = append(append([]byte{}, magic...), nonce...)
	return header, gcm.Seal(plaintext[:0], nonce, plaintext, aad), nil
}

// Open decrypts what Seal produced with the same aad.
func (k Key) Open(sealed, aad []byte) ([]byte, error) {
	return k.open(nil, sealed, aad)
}

// OpenInPlace is Open that decrypts into sealed's own storage, so a body is
// held once: sealed is overwritten (and holds nothing usable after an
// error), and the plaintext returned shares its memory.
func (k Key) OpenInPlace(sealed, aad []byte) ([]byte, error) {
	return k.open(sealed, sealed, aad)
}

func (k Key) open(dst, sealed, aad []byte) ([]byte, error) {
	gcm, err := k.gcm()
	if err != nil {
		return nil, err
	}
	n := len(magic) + gcm.NonceSize()
	if len(sealed) < n || !bytes.Equal(sealed[:len(magic)], magic) {
		return nil, errors.New("not a DriftGate encrypted file")
	}
	if dst != nil {
		dst = sealed[n:n] // the ciphertext's storage, exactly
	}
	pt, err := gcm.Open(dst, sealed[len(magic):n], sealed[n:], aad)
	if err != nil {
		return nil, errors.New("decryption failed: wrong key or corrupted file")
	}
	return pt, nil
}

// refLabel separates the key that names private bodies from the key that
// encrypts them.
const refLabel = "driftgate private ref v1"

// Ref names a private body without revealing it: the HMAC-SHA256 of the
// plaintext under a key derived from k. Equal bodies get equal names under one
// key, but a guess of a short body cannot be checked against the name
// without the key, as it could against a plain SHA-256.
func (k Key) Ref(plaintext []byte) string {
	mac := k.RefHash()
	mac.Write(plaintext)
	return hex.EncodeToString(mac.Sum(nil))
}

// RefHash is Ref for content read in parts: its sum, in hex, is the ref of
// what was written to it.
func (k Key) RefHash() hash.Hash {
	derive := hmac.New(sha256.New, k)
	derive.Write([]byte(refLabel))
	return hmac.New(sha256.New, derive.Sum(nil))
}

func (k Key) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
