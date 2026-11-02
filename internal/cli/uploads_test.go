package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/sheet"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// uploadsCatalog checks uploads, a queued job, exports and a rate limit on
// the demo: two real files uploaded (CSV and XLSX, from the uploads folder
// outside the repository) and a synthetic one, the queued import job run by
// a command step, the imported rows read back, the export in both formats,
// and 61 likes against a limit of 60 per minute.
const uploadsCatalog = `{"version":1,"requests":[
 {"id":"import-csv","method":"POST","path":"/api/imports","identity":"admin","mutates":true,"storage":"private",
  "multipart":{"fields":[["kind","orders"]],"files":[{"field":"file","path":"orders/2024.csv","ref":"%s"}]}},
 {"id":"import-xlsx","method":"POST","path":"/api/imports","identity":"admin","mutates":true,"storage":"private",
  "multipart":{"fields":[["kind","orders"]],"files":[{"field":"file","path":"orders/2025.xlsx","ref":"%s"}]}},
 {"id":"import-synthetic","method":"POST","path":"/api/imports","identity":"admin","mutates":true,"storage":"public",
  "multipart":{"fields":[["kind","orders"]],"files":[{"field":"file","synthetic":"xlsx"}]}},
 {"id":"import-no-file","method":"POST","path":"/api/imports","identity":"admin","mutates":true,"storage":"public",
  "multipart":{"fields":[["kind","orders"]]}},
 {"id":"queue-work","command":["curl","-s","-f","-X","POST","%s/admin/queue/work"],"output":true,"storage":"public"},
 {"id":"orders","method":"GET","path":"/api/orders","storage":"public"},
 {"id":"export-xlsx","method":"GET","path":"/api/exports/orders.xlsx","storage":"public"},
 {"id":"export-csv","method":"GET","path":"/api/exports/orders.csv","storage":"public"},
 {"id":"like-throttle","method":"PUT","path":"/api/posts/12/like","repeat":61,"storage":"public"}],
 "sequences":[{"id":"imports","requests":["import-csv","import-xlsx","import-synthetic","import-no-file","queue-work","orders","export-xlsx","export-csv"]}]}`

const uploadsAllowlist = `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
  - name: retry-after
    format: '\d+'
    reason: Seconds until the rate limit resets.
  - name: x-ratelimit-reset
    format: '\d{10}'
    reason: Time the rate limit resets.
`

// TestUploadsRepeatAndCommands: run makes a
// baseline of uploads, a queued job, exports and a rate limit, finds no
// drift on the same code, and each planted change gives drift at its place.
func TestUploadsRepeatAndCommands(t *testing.T) {
	api := demo.New("v1")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	uploads := t.TempDir() // outside the project, like ~/Data/<project>
	write(t, filepath.Join(uploads, "orders", "2024.csv"), "region,value\nNorth,12.5\nSouth,7.25\n")
	xlsx := sheet.WriteXLSX([][]string{{"region", "value"}, {"West", "30.75"}, {"East", "4"}})
	if err := os.WriteFile(filepath.Join(uploads, "orders", "2025.xlsx"), xlsx, 0o644); err != nil {
		t.Fatal(err)
	}
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	key := secret.Key(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	t.Setenv("DG_DEMO_ADMIN", demo.AdminToken)
	csvRef := key.Ref([]byte("region,value\nNorth,12.5\nSouth,7.25\n"))

	cfg := filepath.Join(root, "driftgate.yaml")
	state := func(name string) string {
		return fmt.Sprintf("  %s:\n    restore: [\"curl\", \"-sf\", \"-X\", \"POST\", \"%s/admin/reset\"]\n    fingerprint: [\"curl\", \"-sf\", \"%s/admin/state/%s\"]\n", name, srv.URL, srv.URL, name)
	}
	// The rate limit of the configuration spaces catalog requests; the sends
	// of a repeated request skip it, or the application's limit would never
	// be reached (61 sends at 50 a minute).
	write(t, cfg, fmt.Sprintf("version: 1\nproject: demo\ntarget:\n  base_url: %s\n  rate_limits:\n    - match: /api/posts/12/like\n      per_minute: 50\nidentities:\n  admin: { env: DG_DEMO_ADMIN }\nuploads:\n  dir: %s\nstate:\n%s%s",
		srv.URL, uploads, state("orders"), state("jobs")))
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), uploadsAllowlist)
	writeCatalog := func(csv, xlsx string) {
		write(t, filepath.Join(root, "driftgate", "catalog.json"), fmt.Sprintf(uploadsCatalog, csv, xlsx, srv.URL))
	}

	// Without the refs the catalog does not say which files it means; the
	// error gives the value after the file was checked.
	writeCatalog("", "")
	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitConfig || !strings.Contains(errOut, `orders/2024.csv has no ref in the catalog; after checking it is the file to upload, set "ref": "`+csvRef+`"`) {
		t.Fatalf("run without refs: %d\n%s\n%s", code, out, errOut)
	}
	writeCatalog(csvRef, key.Ref(xlsx))

	code, out, errOut = runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "baseline local made from two equal captures") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(root, "driftgate", "baselines", "local", "responses")
	rec := func(id string) *snapshot.Record {
		raw, err := os.ReadFile(filepath.Join(base, id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var r snapshot.Record
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	if r := rec("like-throttle"); r.Status != 429 || r.Repeat != "200×60, 429×1" {
		t.Fatalf("throttled request: %d %q", r.Status, r.Repeat)
	}
	if r := rec("queue-work"); r.Exit == nil || *r.Exit != 0 || strings.Join(r.SideEffects.Items, " ") != "jobs/jobs orders/orders" {
		t.Fatalf("command step: %+v %+v", r.Exit, r.SideEffects)
	}
	if r := rec("import-csv"); r.Status != 202 || r.Body.Storage != snapshot.Private || r.Body.SHA256 != "" {
		t.Fatalf("upload of a real file: %+v", r)
	}
	if r := rec("import-no-file"); r.Status != 422 {
		t.Fatalf("upload without a file: %d", r.Status)
	}
	if r := rec("orders"); r.Body.Size == 0 {
		t.Fatal("imported rows not read back")
	}
	// Nothing of the real files is in the repository.
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "South") || strings.Contains(string(raw), "West") {
				t.Errorf("%s holds content of an uploaded file", path)
			}
		}
		return nil
	})

	if code, out, errOut = runCLI(t, "run", "--config", cfg); code != ExitClean || !strings.Contains(out, "No drift") {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}

	type place struct{ request, kind, where string }
	check := func(change string, want ...place) {
		t.Helper()
		api.Plant(change, true)
		defer api.Plant(change, false)
		code, out, errOut := runCLI(t, "run", "--config", cfg)
		if code != ExitDrift {
			t.Fatalf("%s: run exit %d\n%s\n%s", change, code, out, errOut)
		}
		_, r := compareJSON(t, cfg, runTagFor(t, cfg), "baseline:local")
		got := map[place]bool{}
		var all []string
		for _, res := range r.Results {
			for _, d := range res.Drifts {
				got[place{d.Request, d.Kind, d.Where}] = true
				all = append(all, fmt.Sprintf("%s %s %s %s → %s", d.Request, d.Kind, d.Where, d.Baseline, d.Current))
			}
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s: no drift %+v; drifts:\n%s", change, w, strings.Join(all, "\n"))
			}
		}
		if t.Failed() {
			t.FailNow()
		}
		t.Logf("%s:\n%s", change, strings.Join(all, "\n"))
	}
	// The import reads 12.5 as 12: the rows read back and both exports.
	check(demo.ChangeUpload,
		place{"orders", compare.KindValue, "$.entries[0].value"},
		place{"export-xlsx", compare.KindCell, "xlsx:Sheet1!B2"},
		place{"export-csv", compare.KindCell, "csv!B2"})
	// The limit drops to 59: the 60th request is already refused.
	check(demo.ChangeLimit,
		place{"like-throttle", compare.KindRepeat, "repeat"},
		place{"like-throttle", compare.KindHeaderValue, "header:x-ratelimit-limit"})
	// The export renames a column.
	check(demo.ChangeExport,
		place{"export-xlsx", compare.KindCell, "xlsx:Sheet1!B1"},
		place{"export-csv", compare.KindCell, "csv!B1"})
	// The queued job fails: the command step's exit code, output and side
	// effects, and the rows that are not there.
	check(demo.ChangeJob,
		place{"queue-work", compare.KindExit, "exit"},
		place{"queue-work", compare.KindBody, "line 1"},
		place{"queue-work", compare.KindSideEffect, "side-effects:orders/orders"},
		place{"orders", compare.KindArrayLength, "$.entries"})

	// A changed input file is a change of configuration, not drift.
	write(t, filepath.Join(uploads, "orders", "2024.csv"), "region,value\nNorth,12.5\n")
	if code, out, errOut = runCLI(t, "run", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "orders/2024.csv is not the file the catalog names") {
		t.Fatalf("run with a changed file: %d\n%s\n%s", code, out, errOut)
	}
	// And a missing one makes the result one that cannot be trusted.
	os.Remove(filepath.Join(uploads, "orders", "2024.csv"))
	if code, _, errOut = runCLI(t, "run", "--config", cfg); code != ExitEnv || !strings.Contains(errOut, "not on this machine") {
		t.Fatalf("run without the file: %d\n%s", code, errOut)
	}
	// Without a restore an uploaded file would stay in the application.
	write(t, filepath.Join(uploads, "orders", "2024.csv"), "region,value\nNorth,12.5\nSouth,7.25\n")
	if code, _, errOut = runCLI(t, "capture", "x", "--config", cfg, "--allow-mutating-without-restore"); code != ExitConfig || !strings.Contains(errOut, "import-csv uploads a file") {
		t.Fatalf("upload with --allow-mutating-without-restore: %d\n%s", code, errOut)
	}
	raw, _ := os.ReadFile(cfg)
	write(t, cfg, string(raw[:strings.Index(string(raw), "state:")]))
	if code, _, errOut = runCLI(t, "capture", "x", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "import-csv uploads a file") {
		t.Fatalf("upload without state: %d\n%s", code, errOut)
	}
	write(t, cfg, string(raw))
	// An uploads folder inside the project is refused.
	t.Setenv("DRIFTGATE_UPLOADS", filepath.Join(root, "uploads"))
	if code, _, errOut = runCLI(t, "run", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "inside the repository") {
		t.Fatalf("run with uploads in the project: %d\n%s", code, errOut)
	}
}

// TestInitLaravelRateLimitHeaders: init proposes allowlist rules for the
// headers of Laravel's throttle middleware.
func TestInitLaravelRateLimitHeaders(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "artisan"), "#!/usr/bin/env php\n")
	write(t, filepath.Join(root, "composer.lock"), `{"packages":[{"name":"laravel/framework","version":"v10.48.0"}]}`)
	cfg := filepath.Join(root, "driftgate.yaml")
	if code, out, errOut := runCLI(t, "init", "--config", cfg); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "driftgate", "allowlist.yaml"))
	for _, want := range []string{"name: x-ratelimit-remaining", "name: x-ratelimit-reset", "name: retry-after"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("allowlist lacks %q:\n%s", want, raw)
		}
	}
	if _, err := allowlist.Parse(raw); err != nil {
		t.Fatalf("the allowlist does not load: %v", err)
	}
}
