package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// noKey is secret.ErrNoKey with its own message.
type noKey struct{ error }

func (noKey) Is(target error) bool { return target == secret.ErrNoKey }

// keyHome is where driftgate key create puts the key of a layout.
func keyHome(l Layout) string {
	if l.Key != "" {
		return rel(l.Root, l.Key)
	}
	return "the macOS Keychain"
}

// KeySource says where LoadKey takes the project key from.
func (p *Project) KeySource() string {
	switch {
	case p.Key.Op != "":
		return "key.op " + p.Key.Op
	case p.Key.Env != "":
		return "key.env " + p.Key.Env
	case os.Getenv(secret.EnvKey) != "":
		return secret.EnvKey
	}
	return keyHome(p.Layout)
}

// KeyConflict says, when DRIFTGATE_KEY is set and holds another key than
// the layout's key file, that the variable wins; "" otherwise.
func (p *Project) KeyConflict() string {
	if p.Key != (secret.Source{}) || p.Layout.Key == "" || os.Getenv(secret.EnvKey) == "" {
		return ""
	}
	file, err := secret.ReadFile(p.Layout.Key)
	if err != nil {
		return ""
	}
	env, err := secret.Load(p.Project, secret.Source{}, "")
	if err != nil || string(env) == string(file) {
		return ""
	}
	return fmt.Sprintf("%s is set and differs from %s; %s is used (unset it to use the key file)", secret.EnvKey, rel(p.Root, p.Layout.Key), secret.EnvKey)
}

// keyHint is the KeyHint of the project's private store.
func (p *Project) keyHint() string {
	if c := p.KeyConflict(); c != "" {
		return c
	}
	return "the key is from " + p.KeySource()
}

// sealedBodies counts the private bodies in the store.
func (p *Project) sealedBodies() int {
	list, _ := os.ReadDir(p.Layout.Private)
	n := 0
	for _, e := range list {
		if strings.HasSuffix(e.Name(), ".enc") {
			n++
		}
	}
	return n
}

// orphans refuses a new key while the store holds private bodies: a new
// key cannot read them. When DRIFTGATE_KEY opens them, it says how to keep
// that key in the key file instead.
func (p *Project) orphans() error {
	n := p.sealedBodies()
	if n == 0 {
		return nil
	}
	list, _ := os.ReadDir(p.Layout.Private)
	var ref string
	for _, e := range list {
		if r, ok := strings.CutSuffix(e.Name(), ".enc"); ok {
			ref = r
			break
		}
	}
	env, err := secret.Load(p.Project, secret.Source{}, "")
	if os.Getenv(secret.EnvKey) != "" && err == nil && p.Layout.Key != "" {
		store := &snapshot.PrivateStore{Dir: p.Layout.Private, Ext: ".enc", Key: func() (secret.Key, error) { return env, nil }}
		if _, err := store.Get(ref); err == nil {
			return invalid{fmt.Errorf("the store holds %d private bodies sealed with the key in %s; a new key could not read them. Keep that key in the key file instead: printf '%%s\\n' \"$%s\" > %s && chmod 600 %s",
				n, secret.EnvKey, secret.EnvKey, p.Layout.Key, p.Layout.Key)}
		}
	}
	return invalid{fmt.Errorf("the store holds %d private bodies (%s) sealed with a key that is not here; a new key could not read them. Put the project key back (%s, %s or key: in %s); if it is lost, remove %s and make the baselines again",
		n, p.Layout.Private, keyHome(p.Layout), secret.EnvKey, FileName, filepath.Clean(p.Layout.Private))}
}
