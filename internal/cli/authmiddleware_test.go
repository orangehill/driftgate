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
	const suspect = `note: routes with App\Http\Middleware\GetUserFromToken get no identity; if it signs a user in, name its class under discover[].auth (e.g. auth: [GetUserFromToken]) and run driftgate discover --write`
	const unused = "note: generate.identity is admin, but no generated request is sent as admin: no route that is not excluded needs authentication"
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 || !strings.Contains(out, suspect) || strings.Contains(out, "Csrf") {
		t.Fatalf("discover --write: %d\n%s\n%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "discover", "--config", cfg); strings.Contains(out, "note:") {
		t.Errorf("discover without --write repeats the note:\n%s", out)
	}
	if _, out, errOut := runCLI(t, "generate", "--config", cfg); !strings.Contains(out, unused) {
		t.Errorf("generate without auth routes:\n%s\n%s", out, errOut)
	}

	// An alias matches nothing: route:list lists it as its class.
	write(t, cfg, base+"    auth: [GetUserFromToken, jwt.auth]\n")
	const unmatched = "note: discover[].auth jwt.auth matches no middleware of a route; route:list --json lists an alias as its class and a middleware group by its name, so name the class as it shows there (with or without its namespace), or the group it is in"
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 || strings.Count(out, "note:") != 1 || !strings.Contains(out, unmatched) {
		t.Errorf("discover --write with auth: %d\n%s\n%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "discover", "--config", cfg); strings.Contains(out, "note:") {
		t.Errorf("discover without --write repeats the note:\n%s", out)
	}
	if code, out, errOut := runCLI(t, "generate", "--write", "--config", cfg); code != 0 || strings.Contains(out, unused) {
		t.Errorf("generate with auth: %d\n%s\n%s", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(root, "driftgate", "catalog.json"))
	if err != nil || !strings.Contains(string(raw), `"identity": "admin"`) {
		t.Errorf("catalog: %v\n%s", err, raw)
	}
}

// TestIdentityUnusedAllTargets: the note on an identity no route needs looks
// at every service, also when --target selects one without such routes.
func TestIdentityUnusedAllTargets(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "a.json"), `[{"method":"GET","path":"/me","auth":true}]`)
	write(t, filepath.Join(root, "b.json"), `[{"method":"GET","path":"/news"}]`)
	write(t, cfg, "version: 1\nproject: shop\ntargets:\n  a:\n    base_url: http://localhost:8000\n  b:\n    base_url: http://localhost:8001\n"+
		"identities:\n  admin: {}\ngenerate:\n  identity: admin\ndiscover:\n  - adapter: command\n    target: a\n    command: [\"cat\", \"a.json\"]\n  - adapter: command\n    target: b\n    command: [\"cat\", \"b.json\"]\n")
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 {
		t.Fatalf("discover: %d\n%s\n%s", code, out, errOut)
	}
	for _, args := range [][]string{{"generate", "--config", cfg}, {"generate", "--config", cfg, "--target", "b"}} {
		if code, out, errOut := runCLI(t, args...); code != 0 || strings.Contains(out, "generate.identity") {
			t.Errorf("%v: %d\n%s\n%s", args, code, out, errOut)
		}
	}
}

// TestAuthMiddlewareGroup: route:list --json lists a middleware group by
// its name, not the middleware in it (only -vv expands groups), so the
// application's own sign-in hides in a group of its own. discover --write
// names the group, generate notes the identity no request carries although
// an excluded route (Horizon's) needs authentication, and the group named
// under discover[].auth gives the requests the identity.
func TestAuthMiddlewareGroup(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "routes-src.json"), `[{"method":"GET|HEAD","uri":"api/profile","middleware":["api","adminpanel"]},`+
		`{"method":"GET|HEAD","uri":"horizon/api/stats","middleware":["web","Laravel\\Horizon\\Http\\Middleware\\Authenticate"]}]`)
	base := "version: 1\nproject: shop\ntarget:\n  base_url: http://localhost:8000\nidentities:\n  admin: {}\ngenerate:\n  identity: admin\ndiscover:\n  - adapter: laravel\n    command: [\"cat\", \"routes-src.json\"]\n"
	write(t, cfg, base)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	write(t, filepath.Join(root, "driftgate", "exclusions.yaml"), "version: 1\nroutes:\n  - route: GET /horizon/api/stats\n    reason: Laravel Horizon dashboard.\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	const group = "note: routes with the middleware group adminpanel get no identity; route:list --json lists a group by its name, not its middleware: if one signs a user in, name it under discover[].auth (e.g. auth: [adminpanel])"
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 || !strings.Contains(out, group) || strings.Contains(out, "group api") {
		t.Fatalf("discover --write: %d\n%s\n%s", code, out, errOut)
	}
	const unused = "note: generate.identity is admin, but no generated request is sent as admin"
	if _, out, errOut := runCLI(t, "generate", "--config", cfg); !strings.Contains(out, unused) {
		t.Errorf("generate with only an excluded route behind auth:\n%s\n%s", out, errOut)
	}
	write(t, cfg, base+"    auth: [adminpanel]\n")
	if code, out, errOut := runCLI(t, "discover", "--write", "--config", cfg); code != 0 || strings.Contains(out, "note:") {
		t.Errorf("discover --write with the group: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "generate", "--write", "--config", cfg); code != 0 || strings.Contains(out, unused) {
		t.Errorf("generate with the group: %d\n%s\n%s", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(root, "driftgate", "catalog.json"))
	if err != nil || !strings.Contains(string(raw), `"identity": "admin"`) {
		t.Errorf("catalog: %v\n%s", err, raw)
	}
}
