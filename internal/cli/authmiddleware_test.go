package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCustomAuthMiddleware: routes behind an application's own auth
// middleware get no identity until discover[].auth names it. discover
// --write says so once (not a later discover), generate notes an identity
// no route uses, and with auth named both stay quiet and the requests carry
// the identity.
func TestCustomAuthMiddleware(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "routes-src.json"), `[{"method":"GET|HEAD","uri":"api/profile","middleware":["api","App\\Http\\Middleware\\GetUserFromToken"]},`+
		`{"method":"POST","uri":"login","middleware":["web","Illuminate\\Foundation\\Http\\Middleware\\ValidateCsrfToken"]}]`)
	base := "version: 1\nproject: shop\ntarget:\n  base_url: http://localhost:8000\nidentities:\n  admin: {}\ngenerate:\n  identity: admin\ndiscover:\n  - adapter: laravel\n    command: [\"cat\", \"routes-src.json\"]\n"
	write(t, cfg, base)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	const suspect = `note: routes with App\Http\Middleware\GetUserFromToken get no identity; if it signs a user in, name it under discover[].auth (alias or class name, e.g. auth: [GetUserFromToken]) and run driftgate discover --write`
	const unused = "note: generate.identity is admin, but no route needs authentication"
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 || !strings.Contains(out, suspect) || strings.Contains(out, "Csrf") {
		t.Fatalf("discover --write: %d\n%s\n%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "discover", "--config", cfg); strings.Contains(out, "note:") {
		t.Errorf("discover without --write repeats the note:\n%s", out)
	}
	if _, out, errOut := runCLI(t, "generate", "--config", cfg); !strings.Contains(out, unused) {
		t.Errorf("generate without auth routes:\n%s\n%s", out, errOut)
	}

	write(t, cfg, base+"    auth: [GetUserFromToken]\n")
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 || strings.Contains(out, "note:") {
		t.Errorf("discover --write with auth: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "generate", "--write", "--config", cfg); code != 0 || strings.Contains(out, unused) {
		t.Errorf("generate with auth: %d\n%s\n%s", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(root, "driftgate", "catalog.json"))
	if err != nil || !strings.Contains(string(raw), `"identity": "admin"`) {
		t.Errorf("catalog: %v\n%s", err, raw)
	}
}
