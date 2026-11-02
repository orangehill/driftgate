package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// privateProject makes the first baseline of the demo API; me and login are
// private, and with newsPrivate news too (it drifts in v2).
func privateProject(t *testing.T, extra string, newsPrivate bool) (*demoProject, string) {
	t.Helper()
	p := newDemoProject(t, extra)
	if newsPrivate {
		write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"),
			strings.Replace(demoCatalog, `"path":"/api/news","storage":"public"`, `"path":"/api/news","storage":"private"`, 1))
	}
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	return p, filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
}

// privateContent decrypts the private bodies of the baseline from the
// local store: request ID → body.
func privateContent(t *testing.T, cfg string) map[string][]byte {
	t.Helper()
	proj, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := proj.Baseline("local")
	recs, err := set.Records()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, r := range recs {
		if r.Body.Storage != snapshot.Private {
			continue
		}
		body, err := set.Body(r)
		if err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
		out[r.ID] = body
	}
	return out
}

// assertNothingPrivate checks that nothing under driftgate/ holds a private
// body: not in plain, not its plain SHA-256, not encrypted (no private/
// folder, no sealed file, no content of a sealed body of the local store).
func assertNothingPrivate(t *testing.T, cfg string, bodies map[string][]byte) {
	t.Helper()
	var needles [][]byte
	for _, b := range bodies {
		needles = append(needles, b, []byte(snapshot.Sum(b)))
	}
	needles = append(needles, []byte("pera.peric"))
	sealed, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "private", "*.enc"))
	for _, f := range sealed {
		raw, _ := os.ReadFile(f)
		needles = append(needles, raw)
	}
	filepath.Walk(filepath.Join(filepath.Dir(cfg), "driftgate"), func(path string, info os.FileInfo, _ error) error {
		if info == nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == repoPrivate {
				t.Errorf("%s: a private/ folder in the repository", path)
			}
			return nil
		}
		raw, _ := os.ReadFile(path)
		if bytes.HasPrefix(raw, []byte("DGE1")) {
			t.Errorf("%s is an encrypted body", path)
		}
		for _, n := range needles {
			if len(n) > 0 && bytes.Contains(raw, n) {
				t.Errorf("%s holds %.40q", path, n)
			}
		}
		return nil
	})
}

func TestFirstRunKeepsPrivateBodiesLocal(t *testing.T) {
	p, base := privateProject(t, "", false)
	bodies := privateContent(t, p.cfg)
	if len(bodies) != 2 || !bytes.Contains(bodies["me"], []byte("pera.peric")) {
		t.Fatalf("private bodies: %q", bodies)
	}
	assertNothingPrivate(t, p.cfg, bodies)
	m, err := baseline.Read(base)
	if err != nil {
		t.Fatal(err)
	}
	for f := range m.Files {
		if strings.HasPrefix(f, "private/") {
			t.Errorf("%s is in the manifest", f)
		}
	}
	code, out, _ := runCLI(t, "verify", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "2 private bodies are outside the repository") || !strings.Contains(out, "2 of them are on this machine") {
		t.Fatalf("verify: %d\n%s", code, out)
	}
}

// comparable is what a comparison found, without times and digests.
func comparable(r *report.Report) []string {
	var out []string
	for _, res := range r.Results {
		out = append(out, res.ID+" "+res.Outcome)
		for _, d := range res.Drifts {
			out = append(out, fmt.Sprintf("%s %s %s %q %q", res.ID, d.Kind, d.Where, d.Baseline, d.Current))
		}
	}
	sort.Strings(out)
	return out
}

// ciStore is CI as the action sets it up: the repository, a store of its own
// and a random key of the job's own.
func ciStore(t *testing.T) {
	t.Helper()
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "ci-store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
}

// TestCompareWithoutPrivateBodies is CI: the private bodies of the baseline
// are not there, so their requests are skipped and listed, and the result
// is incomplete (exit 3) unless --skip-private accepts a partial check.
func TestCompareWithoutPrivateBodies(t *testing.T) {
	p, _ := privateProject(t, "", true)

	// Where the bodies are, everything is compared: login passes because of
	// the allowlist rule for $.token inside its private body.
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean || strings.Contains(out, "SKIP") {
		t.Fatalf("run where the bodies are: %d\n%s\n%s", code, out, errOut)
	}
	reportExits(t, p.cfg, ExitClean)
	proj, _ := config.Load(p.cfg)
	cur, _ := proj.Capture(runTag(proj))
	base, _ := proj.Baseline("local")
	rc, _ := cur.Record("login")
	rb, _ := base.Record("login")
	if rc.Body.Ref == rb.Body.Ref {
		t.Fatal("login returned the same token twice; the allowlist rule was not tested")
	}
	_, local := compareJSON(t, p.cfg, runTagFor(t, p.cfg), "local")

	ciStore(t)
	code, out, errOut := runCLI(t, "run", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(out, "the result is incomplete: 3 private requests could not be compared here") {
		t.Fatalf("run in CI: %d\n%s\n%s", code, out, errOut)
	}
	reportExits(t, p.cfg, code)
	for _, id := range []string{"news", "me", "login"} {
		if !strings.Contains(out, "SKIP   "+id+" (private body not on this machine)") {
			t.Errorf("%s is not listed as skipped:\n%s", id, out)
		}
	}
	if strings.Contains(out, "No drift") {
		t.Errorf("an incomplete result says no drift:\n%s", out)
	}
	_, ci := compareJSON(t, p.cfg, runTagFor(t, p.cfg), "local")
	if !reflect.DeepEqual(ci.PrivateMissing, []string{"news", "me", "login"}) || ci.SkipPrivate || !ci.Incomplete() {
		t.Fatalf("report: %+v", ci)
	}
	// The public requests got the same result as where the bodies are.
	var pub, pubLocal []string
	for _, l := range comparable(ci) {
		if !strings.HasPrefix(l, "news ") && !strings.HasPrefix(l, "me ") && !strings.HasPrefix(l, "login ") {
			pub = append(pub, l)
		}
	}
	for _, l := range comparable(local) {
		if !strings.HasPrefix(l, "news ") && !strings.HasPrefix(l, "me ") && !strings.HasPrefix(l, "login ") {
			pubLocal = append(pubLocal, l)
		}
	}
	if !reflect.DeepEqual(pub, pubLocal) {
		t.Fatalf("public results differ:\n%s\n---\n%s", strings.Join(pub, "\n"), strings.Join(pubLocal, "\n"))
	}
	if _, md, _ := runCLI(t, "report", "--format", "markdown", "--config", p.cfg); !strings.Contains(md, "incomplete, 3 private requests not compared here") {
		t.Fatalf("markdown:\n%s", md)
	}

	// --skip-private: the public rest decides.
	code, out, errOut = runCLI(t, "run", "--skip-private", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "3 private requests compared without the body with --skip-private") {
		t.Fatalf("run in CI with --skip-private: %d\n%s\n%s", code, out, errOut)
	}
	reportExits(t, p.cfg, code)
	// What is in the committed record is compared without the body: a
	// private request that now fails (200 → 401) drifts.
	t.Setenv("DG_DEMO_ADMIN", "not-the-token")
	code, out, errOut = runCLI(t, "run", "--skip-private", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "DRIFT  me") || !strings.Contains(out, "compared without the body") {
		t.Fatalf("private status change in CI with --skip-private: %d\n%s\n%s", code, out, errOut)
	}
	reportExits(t, p.cfg, code)
	if code, out, _ := runCLI(t, "run", "--config", p.cfg); code != ExitEnv || !strings.Contains(out, "DRIFT  me") {
		t.Fatalf("private status change in CI: %d\n%s", code, out)
	}
	t.Setenv("DG_DEMO_ADMIN", demo.AdminToken)
	p.api.SetVersion("v2")
	code, out, errOut = runCLI(t, "run", "--skip-private", "--config", p.cfg)
	// The private news drifts in its new header, compared without its body.
	if code != ExitDrift || !strings.Contains(out, "DRIFT  news-item") || !strings.Contains(out, "DRIFT  news\n") {
		t.Fatalf("v2 in CI with --skip-private: %d\n%s\n%s", code, out, errOut)
	}
	// Without it, missing private bodies outweigh drift: the check is not done.
	if code, out, _ := runCLI(t, "run", "--config", p.cfg); code != ExitEnv || !strings.Contains(out, "DRIFT  news-item") {
		t.Fatalf("v2 in CI: %d\n%s", code, out)
	}
	// Incomplete and drifted: 3, as run.
	reportExits(t, p.cfg, ExitEnv)
	// verify checks the repository only; the bodies are simply not here.
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "3 private bodies are outside the repository") || !strings.Contains(out, "0 of them are on this machine") {
		t.Fatalf("verify in CI: %d\n%s", code, out)
	}
}

// TestUnchangedPrivateBodyWithProjectKey: with the project key, a private
// body the capture returns unchanged has the baseline's ref, so it is on
// this machine and compared; one that changed is not.
func TestUnchangedPrivateBodyWithProjectKey(t *testing.T) {
	p, _ := privateProject(t, "", false)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "ci-store"))
	code, out, _ := runCLI(t, "run", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(out, "SKIP   login (private body not on this machine)") || strings.Contains(out, "SKIP   me") {
		t.Fatalf("run with the project key in a fresh store: %d\n%s", code, out)
	}
}

func TestComparePrivateWithoutKey(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	// A key source of its own: without it set, nothing falls back to the Keychain.
	t.Setenv("DG_TEST_KEY", base64.StdEncoding.EncodeToString(k))
	p, _ := privateProject(t, "key:\n  env: DG_TEST_KEY\n", false)
	t.Setenv("DG_TEST_KEY", "")

	code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(errOut, "baseline:local has private bodies") || !strings.Contains(errOut, "DRIFTGATE_KEY") {
		t.Fatalf("compare without key: %d %s", code, errOut)
	}
	// --skip-private does not replace the key: the capture needs it too.
	if code, _, _ := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--skip-private", "--config", p.cfg); code != ExitEnv {
		t.Fatalf("compare without key, --skip-private: %d", code)
	}
	// run stops before it captures anything.
	code, out, errOut := runCLI(t, "run", "--config", p.cfg)
	if code != ExitEnv || strings.Contains(out, "== capture ==") || !strings.Contains(errOut, "DRIFTGATE_KEY") {
		t.Fatalf("run without key: %d\n%s\n%s", code, out, errOut)
	}
	// Only public requests need no key.
	if code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--only", "news*", "--config", p.cfg); code != ExitClean {
		t.Fatalf("public only: %d %s", code, errOut)
	}
	// The manifest is checked without the key.
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean {
		t.Fatalf("verify without key: %d\n%s", code, out)
	}
	// A wrong key is found too.
	rand.Read(k)
	t.Setenv("DG_TEST_KEY", base64.StdEncoding.EncodeToString(k))
	if code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "wrong key") {
		t.Fatalf("compare with a wrong key: %d %s", code, errOut)
	}
}

func TestAcceptKeepsPrivateBodiesLocal(t *testing.T) {
	p, base := privateProject(t, "", true)
	setMode(t, p.cfg, "evolve")
	before := privateContent(t, p.cfg)

	p.api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitDrift {
		t.Fatalf("v2: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "accept", "news", "me", "--reason", "v2", "--config", p.cfg); code != ExitClean {
		t.Fatalf("accept: %d\n%s\n%s", code, out, errOut)
	}
	after := privateContent(t, p.cfg)
	if bytes.Equal(before["news"], after["news"]) || !bytes.Contains(after["news"], []byte(`"meta"`)) {
		t.Fatalf("news body not accepted: %s", after["news"])
	}
	rec, _ := (&snapshot.Set{Dir: base}).Record("news")
	if rec.Body.Ref == "" || rec.Body.SHA256 != "" {
		t.Fatalf("news record: %+v", rec.Body)
	}
	assertNothingPrivate(t, p.cfg, after)
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean {
		t.Fatalf("verify: %d\n%s", code, out)
	}
	// Accepted requests pass; the others still drift.
	runCLI(t, "run", "--config", p.cfg)
	if _, r := compareJSON(t, p.cfg, runTagFor(t, p.cfg), "local"); outcome(r, "news") != report.Pass || outcome(r, "me") != report.Pass || outcome(r, "login") != report.Drift {
		t.Fatalf("after accept: %+v", r.Results)
	}
}

// A private body missing from the local store is not a problem of the
// baseline: verify passes and counts it, compare skips its request.
func TestMissingPrivateBody(t *testing.T) {
	p, _ := privateProject(t, "", false)
	proj, _ := config.Load(p.cfg)
	base, _ := proj.Baseline("local")
	me, _ := base.Record("me")
	path := filepath.Join(base.Private.Dir, me.Body.Ref+".enc")
	orig, _ := os.ReadFile(path)

	// A changed body in the store is found when it is read.
	changed := append([]byte{}, orig...)
	changed[len(changed)-1] ^= 1
	os.WriteFile(path, changed, 0o600)
	if code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "me:") {
		t.Fatalf("compare with a changed private body: %d %s", code, errOut)
	}

	os.Remove(path)
	code, out, _ := runCLI(t, "verify", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "1 of them are on this machine") {
		t.Fatalf("verify without one body: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(out, "SKIP   me (private body not on this machine)") || !strings.Contains(out, "1 private request could not be compared here") {
		t.Fatalf("compare without one body: %d\n%s", code, out)
	}
}

// makePlainHash turns a baseline into one made before keyed refs: records
// name their private bodies by the plain SHA-256, and the bodies are in the
// local store under that name only.
func makePlainHash(t *testing.T, cfg, base string) map[string][]byte {
	t.Helper()
	bodies := privateContent(t, cfg)
	proj, _ := config.Load(cfg)
	key, err := proj.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	local, _ := proj.Private()
	set := &snapshot.Set{Dir: base}
	for id, body := range bodies {
		sum := snapshot.Sum(body)
		sealed, err := key.Seal(body, []byte(sum))
		if err != nil {
			t.Fatal(err)
		}
		if err := snapshot.WriteFile(filepath.Join(local.Dir, sum+".enc"), sealed, 0o600); err != nil {
			t.Fatal(err)
		}
		r, _ := set.Record(id)
		os.Remove(filepath.Join(local.Dir, r.Body.Ref+".enc"))
		r.Body.SHA256, r.Body.Ref = sum, ""
		if err := snapshot.WriteJSON(filepath.Join(base, "responses", id+".json"), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := baseline.Write(base); err != nil {
		t.Fatal(err)
	}
	return bodies
}

// makeInRepo turns a baseline into one that carries its private bodies in
// the repository, encrypted in private/<ref>, as commit 983ea33 made them;
// the local store does not hold them.
func makeInRepo(t *testing.T, cfg, base string) map[string][]byte {
	t.Helper()
	bodies := privateContent(t, cfg)
	proj, _ := config.Load(cfg)
	key, err := proj.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	local, _ := proj.Private()
	set := &snapshot.Set{Dir: base}
	for id := range bodies {
		r, _ := set.Record(id)
		sealed, err := key.Seal(bodies[id], []byte(r.Body.Ref))
		if err != nil {
			t.Fatal(err)
		}
		if err := snapshot.WriteFile(filepath.Join(base, repoPrivate, r.Body.Ref), sealed, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(local.Dir, r.Body.Ref+".enc"))
	}
	if err := baseline.Write(base); err != nil {
		t.Fatal(err)
	}
	return bodies
}

func TestMigratePlainHash(t *testing.T) {
	p, base := privateProject(t, "", false)
	bodies := makePlainHash(t, p.cfg, base)
	proj, _ := config.Load(p.cfg)
	local, _ := proj.Private()

	code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--config", p.cfg)
	if code != ExitConfig || !strings.Contains(errOut, "--migrate-private") {
		t.Fatalf("compare with an old baseline: %d %s", code, errOut)
	}
	code, out, _ := runCLI(t, "verify", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "2 private bodies are named by their plain SHA-256") || !strings.Contains(out, "--migrate-private") {
		t.Fatalf("verify an old baseline: %d\n%s", code, out)
	}

	// A body missing from the local store: refused, with the request, and nothing changes.
	meFile := filepath.Join(local.Dir, snapshot.Sum(bodies["me"])+".enc")
	os.Rename(meFile, meFile+".away")
	digest, _ := baseline.Digest(base)
	code, _, errOut = runCLI(t, "verify", "--baseline", "local", "--migrate-private", "--config", p.cfg)
	if code != ExitConfig || !strings.Contains(errOut, "\n  me\n") || strings.Contains(errOut, "login") {
		t.Fatalf("migrate with a missing body: %d %s", code, errOut)
	}
	if d, _ := baseline.Digest(base); d != digest {
		t.Fatal("a refused migration changed the baseline")
	}
	os.Rename(meFile+".away", meFile)

	// A baseline that does not verify is refused.
	rec := filepath.Join(base, "responses", "news.json")
	orig, _ := os.ReadFile(rec)
	os.WriteFile(rec, append(orig, ' '), 0o644)
	if code, _, errOut := runCLI(t, "verify", "--baseline", "local", "--migrate-private", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "does not verify") {
		t.Fatalf("migrate an edited baseline: %d %s", code, errOut)
	}
	os.WriteFile(rec, orig, 0o644)

	code, out, errOut = runCLI(t, "verify", "--baseline", "local", "--migrate-private", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "named 2 private bodies") || strings.Contains(out, "moved") {
		t.Fatalf("migrate: %d\n%s\n%s", code, out, errOut)
	}
	log, err := baseline.Log(base)
	if err != nil || len(log) != 1 || log[0].Reason != keyedRefReason || !reflect.DeepEqual(log[0].Requests, []string{"login", "me"}) {
		t.Fatalf("log: %v %+v", err, log)
	}
	if got := privateContent(t, p.cfg); !reflect.DeepEqual(got, bodies) {
		t.Fatal("the bodies changed on the way to keyed refs")
	}
	assertNothingPrivate(t, p.cfg, bodies)
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "2 of them are on this machine") {
		t.Fatalf("verify after migrate: %d\n%s", code, out)
	}
	if code, _, errOut := runCLI(t, "verify", "--baseline", "local", "--migrate-private", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "nothing to migrate") {
		t.Fatalf("migrate twice: %d %s", code, errOut)
	}
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("run after migrate: %d\n%s\n%s", code, out, errOut)
	}
}

func TestMigrateMovesPrivateBodiesOutOfRepository(t *testing.T) {
	p, base := privateProject(t, "", false)
	bodies := makeInRepo(t, p.cfg, base)

	// Such a baseline is refused until it is migrated, also by run.
	code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--against", "local", "--config", p.cfg)
	if code != ExitConfig || !strings.Contains(errOut, "carries private bodies in the repository") || !strings.Contains(errOut, "--migrate-private") {
		t.Fatalf("compare: %d %s", code, errOut)
	}
	if code, out, _ := runCLI(t, "run", "--config", p.cfg); code != ExitConfig || strings.Contains(out, "== capture ==") {
		t.Fatalf("run: %d\n%s", code, out)
	}
	code, out, _ := runCLI(t, "verify", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "carries private bodies in the repository") || strings.Contains(out, "restore it from git") {
		t.Fatalf("verify: %d\n%s", code, out)
	}

	code, out, errOut = runCLI(t, "verify", "--baseline", "local", "--migrate-private", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "moved 2 private bodies of baseline local out of the repository") {
		t.Fatalf("migrate: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(base, repoPrivate)); !os.IsNotExist(err) {
		t.Fatalf("private/ is still in the baseline: %v", err)
	}
	m, _ := baseline.Read(base)
	for f := range m.Files {
		if strings.HasPrefix(f, "private/") {
			t.Errorf("%s is still in the manifest", f)
		}
	}
	log, err := baseline.Log(base)
	if err != nil || len(log) != 1 || log[0].Reason != movedOutReason || !reflect.DeepEqual(log[0].Requests, []string{"login", "me"}) {
		t.Fatalf("log: %v %+v", err, log)
	}
	if got := privateContent(t, p.cfg); !reflect.DeepEqual(got, bodies) {
		t.Fatal("the bodies changed on the way out of the repository")
	}
	assertNothingPrivate(t, p.cfg, bodies)
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "2 of them are on this machine") {
		t.Fatalf("verify after migrate: %d\n%s", code, out)
	}
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("run after migrate: %d\n%s\n%s", code, out, errOut)
	}
}

// A baseline of both kinds: one migration does both, with both reasons.
func TestMigrateBoth(t *testing.T) {
	p, base := privateProject(t, "", false)
	bodies := makeInRepo(t, p.cfg, base)
	proj, _ := config.Load(p.cfg)
	key, _ := proj.LoadKey()
	local, _ := proj.Private()
	// me goes back to its plain SHA-256, in the local store.
	set := &snapshot.Set{Dir: base}
	r, _ := set.Record("me")
	os.Remove(filepath.Join(base, repoPrivate, r.Body.Ref))
	sum := snapshot.Sum(bodies["me"])
	sealed, _ := key.Seal(bodies["me"], []byte(sum))
	snapshot.WriteFile(filepath.Join(local.Dir, sum+".enc"), sealed, 0o600)
	r.Body.SHA256, r.Body.Ref = sum, ""
	snapshot.WriteJSON(filepath.Join(base, "responses", "me.json"), r)
	baseline.Write(base)

	code, out, errOut := runCLI(t, "verify", "--baseline", "local", "--migrate-private", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "moved 1 private bodies") || !strings.Contains(out, "named 1 private bodies") {
		t.Fatalf("migrate: %d\n%s\n%s", code, out, errOut)
	}
	log, _ := baseline.Log(base)
	if len(log) != 1 || log[0].Reason != movedOutReason+"; "+keyedRefReason {
		t.Fatalf("log: %+v", log)
	}
	if got := privateContent(t, p.cfg); !reflect.DeepEqual(got, bodies) {
		t.Fatal("the bodies changed")
	}
	assertNothingPrivate(t, p.cfg, bodies)
}
