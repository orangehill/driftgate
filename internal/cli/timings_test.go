package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
)

// TestCompareTimingsAndKeyedArrays: response times are shown and never
// change the exit code; an array the allowlist pairs by key names its
// items by key.
func TestCompareTimingsAndKeyedArrays(t *testing.T) {
	project, cfg := setsFixture(t)
	allow := filepath.Join(project, "driftgate", "allowlist.yaml")
	raw, _ := os.ReadFile(allow)
	write(t, allow, string(raw)+"arrays:\n  - path: $.entries\n    key: id\n    reason: news by id\n")

	// Without timings on a side: no section.
	code, out, _ := runCLI(t, "compare", "l56", "--config", cfg, "--format", "json")
	var r report.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if code != ExitDrift || r.Timings != nil {
		t.Fatalf("no timings: exit %d, %+v", code, r.Timings)
	}
	found := false
	for _, res := range r.Results {
		for _, d := range res.Drifts {
			found = found || d.Kind == "type" && d.Where == `$.entries[id="5"].id`
		}
	}
	if !found {
		t.Errorf("keyed item path missing: %s", out)
	}

	p, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	capture, _ := p.Capture("l56")
	// A baseline made by run carries the capture's timings.json in its manifest.
	local := filepath.Join(project, "driftgate", "baselines", "local")
	write(t, filepath.Join(local, "timings.json"), `{"pub-news":100,"api-me":40,"mut-login":50}`)
	if err := baseline.Write(local); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(capture.Dir, "timings.json"), `{"pub-news":900,"api-me":45,"mut-login":50}`)
	code, out, errOut := runCLI(t, "compare", "l56", "--config", cfg)
	if code != ExitDrift || !strings.Contains(out, "response times (information, never a gate): p50 50 → 50 ms, p95 100 → 900 ms over 3 requests; 1 request at least 2× and 200 ms slower: pub-news 100 → 900 ms") {
		t.Errorf("text: %d %s\n%s", code, errOut, out)
	}
	if _, out, _ := runCLI(t, "report", "--config", cfg, "--format", "markdown"); !strings.Contains(out, "pub-news 100 → 900 ms") {
		t.Errorf("markdown:\n%s", out)
	}
	// Slower but the same responses: still clean.
	l55, _ := p.Capture("l55")
	write(t, filepath.Join(l55.Dir, "timings.json"), `{"pub-news":5000,"api-me":4000,"mut-login":5000}`)
	if code, out, _ := runCLI(t, "compare", "l55", "--config", cfg); code != ExitClean || !strings.Contains(out, "3 requests at least 2×") {
		t.Errorf("clean but slower: %d\n%s", code, out)
	}
}

func TestHidePath(t *testing.T) {
	for in, want := range map[string]string{
		`$.data[id=42].name`:             `$.data[id=(private)].name`,
		`$.data[user.id="a@b.c"]["x.y"]`: `$.data[user.id=(private)]["(private)"]`,
		`$.data[3].name`:                 `$.data[3].name`,
		// Key names that are not plain are quoted in the step (keyName),
		// and the value is hidden whatever the name holds.
		`$.data["@id"="alice@x.com"].name`:         `$.data["@id"=(private)].name`,
		`$.data["user id"="alice@x.com"]`:          `$.data["user id"=(private)]`,
		`$.data["ključ"=7]`:                        `$.data["ključ"=(private)]`,
		`$.data["a]=b"="x"]`:                       `$.data["a]=b"=(private)]`,
		`$.a[id="x\"]"].b[slug="y=\"z"].c`:         `$.a[id=(private)].b[slug=(private)].c`,
		`$.a["@id"="[\"q\"]"]["k"][user.id=-3][0]`: `$.a["@id"=(private)]["(private)"][user.id=(private)][0]`,
	} {
		if got := hidePath(in); got != want {
			t.Errorf("hidePath(%s) = %s", in, got)
		}
	}
}
