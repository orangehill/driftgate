package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/state"
)

// TestSeedRecord: a seed that fails before it replaces the pristine copy
// keeps its record; a pristine database a seed made before seeds marked it
// (an earlier release) is replaced through its record, and one without either is never
// dropped.
func TestSeedRecord(t *testing.T) {
	p := newDBProject(t)
	seed := func() (int, string) {
		code, _, errOut := runCLI(t, "seed", "--config", p.cfg)
		return code, errOut
	}
	if code, errOut := seed(); code != ExitClean {
		t.Fatalf("seed: exit %d %s", code, errOut)
	}
	records, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "pristine", "db-*.json"))
	if len(records) != 1 {
		t.Fatalf("pristine records %v", records)
	}
	record := records[0]

	// As a seed of an earlier release left it: no mark, and a record without a server
	// under the old name, which is taken over.
	if _, err := p.db.Exec("DROP TABLE dgt_demo_pristine.driftgate_pristine"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "server")
	raw, _ = json.Marshal(m)
	legacy := filepath.Join(filepath.Dir(record), "db.json")
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if code, errOut := seed(); code != ExitClean {
		t.Fatalf("seed over an adopted pristine database: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the record of an earlier release is still there: %v", err)
	}

	// A seed that fails before it replaces the pristine copy keeps the
	// copy and its record (a seed stopped after that is the crash tests').
	if _, err := p.db.Exec("DROP DATABASE dgt_demo"); err != nil {
		t.Fatal(err)
	}
	if code, errOut := seed(); code != ExitEnv || !strings.Contains(errOut, "does not exist") {
		t.Fatalf("seed without the working database: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("a seed that failed before it replaced the copy removed its record: %v", err)
	}

	// Without the mark and the record the database is someone's.
	if _, err := p.db.Exec("CREATE DATABASE dgt_demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec("DROP TABLE dgt_demo_pristine.driftgate_pristine"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if code, errOut := seed(); code != ExitEnv || !strings.Contains(errOut, "was not made by driftgate seed") {
		t.Fatalf("seed over an unmarked database: exit %d %s", code, errOut)
	}
}

// TestFingerprintFormat: a pristine record or a baseline of format 1
// keeps working while its fingerprint matches; where the new format hashes
// an item another way, the message says what to do.
func TestFingerprintFormat(t *testing.T) {
	p := newDBProject(t)
	if _, err := p.db.Exec("CREATE VIEW faq_questions AS SELECT id, created_at FROM faq"); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "seed", "--config", p.cfg); code != ExitClean {
		t.Fatalf("seed: exit %d %s", code, errOut)
	}
	// rewrite edits a JSON file as format 1 wrote it: no format, and
	// the view's hash (state for a meta.json) of the old format.
	rewrite := func(path, key string, stale bool) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "format")
		delete(m, "state_format")
		if stale {
			f := m[key].(map[string]any)
			if key == "state" {
				f = f["db"].(map[string]any)
			}
			f["view:faq_questions"] = "stale"
		}
		raw, _ = json.Marshal(m)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	records, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "pristine", "db-*.json"))
	if len(records) != 1 {
		t.Fatalf("pristine records %v", records)
	}
	record := records[0]

	rewrite(record, "fingerprint", false)
	p.capture(t, "c1")

	rewrite(record, "fingerprint", true)
	code, _, errOut := runCLI(t, "capture", "c2", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(errOut, "re-seed needed: the fingerprint format changed") || !strings.Contains(errOut, "view:faq_questions") {
		t.Fatalf("capture with a pristine record of format 1: exit %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "seed", "--config", p.cfg); code != ExitClean {
		t.Fatalf("seed: exit %d %s", code, errOut)
	}

	base := filepath.Join(p.root, "driftgate", "baselines", "local")
	if err := os.CopyFS(base, os.DirFS(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", "c1"))); err != nil {
		t.Fatal(err)
	}
	rewrite(filepath.Join(base, "meta.json"), "state", false)
	if err := writeManifest(base); err != nil {
		t.Fatal(err)
	}
	p.capture(t, "c3")
	rewrite(filepath.Join(base, "meta.json"), "state", true)
	if err := writeManifest(base); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI(t, "capture", "c4", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(errOut, "older format") || !strings.Contains(errOut, "driftgate verify --baseline local --migrate-state") {
		t.Fatalf("capture against a baseline of format 1: exit %d %s", code, errOut)
	}

	// The migration proves the data is the baseline's in the baseline's
	// own format: a hash format 1 never made is other data.
	if code, out, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--migrate-state"); code != ExitEnv || !strings.Contains(errOut, "also as its own fingerprint format hashes it") || !strings.Contains(errOut, "db/view:faq_questions") {
		t.Fatalf("migrate-state of other data: exit %d\n%s\n%s", code, out, errOut)
	}
	setLegacyView(t, filepath.Join(base, "meta.json"), "state")
	if err := writeManifest(base); err != nil {
		t.Fatal(err)
	}
	logBefore, _ := os.ReadFile(filepath.Join(base, baseline.LogFile))
	if code, out, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--migrate-state"); code != ExitClean || !strings.Contains(out, "now in format 2") {
		t.Fatalf("migrate-state: exit %d\n%s\n%s", code, out, errOut)
	}
	logAfter, _ := os.ReadFile(filepath.Join(base, baseline.LogFile))
	if !strings.HasPrefix(string(logAfter), string(logBefore)) || !strings.Contains(string(logAfter), stateFormatReason) {
		t.Fatalf("acceptance log:\n%s", logAfter)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean {
		t.Fatalf("verify after migrate-state: exit %d\n%s", code, out)
	}
	p.capture(t, "c5")
	if code, _, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--migrate-state"); code != ExitConfig || !strings.Contains(errOut, "nothing to migrate") {
		t.Fatalf("a second migrate-state: exit %d %s", code, errOut)
	}
}

// setLegacyView writes, into a JSON file DriftGate wrote (a pristine record:
// key fingerprint; a meta.json: key state), the view's hash as format 1
// made it, without the format.
func setLegacyView(t *testing.T, path, key string) {
	t.Helper()
	m, err := state.NewMySQL(state.Options{Name: "db", DSN: os.Getenv("DG_TEST_DSN"), Working: "dgt_demo", Pristine: "dgt_demo_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	legacy, err := m.FingerprintAs(context.Background(), 1, []string{"view:faq_questions"})
	if err != nil || legacy["view:faq_questions"] == "" {
		t.Fatalf("legacy fingerprint %v %v", legacy, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "format")
	delete(doc, "state_format")
	f := doc[key].(map[string]any)
	if key == "state" {
		f = f["db"].(map[string]any)
	}
	f["view:faq_questions"] = legacy["view:faq_questions"]
	raw, _ = json.MarshalIndent(doc, "", " ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
