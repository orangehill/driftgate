package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// perfSize is the size of the body of request i: five large bodies (20, 16,
// 12, 8 and 5 MB), forty of 100 KB to 1 MB and small ones otherwise, all
// divided by scale.
func perfSize(i, scale int) int {
	switch i {
	case 7:
		return 20 << 20 / scale
	case 107:
		return 16 << 20 / scale
	case 207:
		return 12 << 20 / scale
	case 307:
		return 8 << 20 / scale
	case 407:
		return 5 << 20 / scale
	}
	if i%15 == 3 {
		return (100<<10 + (i*7919)%(900<<10)) / scale
	}
	return 1<<10 + (i*104729)%(30<<10)/scale
}

// perfBody is the body of request i in capture a or b. b changes the time in
// every body (allow-listed) and, in some bodies, a value, the order of keys,
// a key, an array length or how a number is written; some bodies stay the
// same, and a few are HTML pages with JSON inside.
func perfBody(i, scale int, cur bool) []byte {
	want := perfSize(i, scale)
	var b bytes.Buffer
	gen := "2026-10-29T10:00:00Z"
	if cur && i%50 != 49 {
		gen = "2026-10-29T11:11:11Z"
	}
	html := i%30 == 11
	if html {
		build := "b1"
		if cur {
			build = "b2"
		}
		fmt.Fprintf(&b, `<html><head><title>r%d</title></head><body><h1>Page %d</h1><script id="__NEXT_DATA__" type="application/json">{"buildId":"%s","props":`, i, i, build)
	}
	fmt.Fprintf(&b, `{"meta":{"generated_at":"%s","page":1,"route":"r%d"},"data":[`, gen, i)
	for k := 0; b.Len() < want; k++ {
		if k > 0 {
			b.WriteByte(',')
		}
		title := fmt.Sprintf("Item %d of route %d", k, i)
		if cur && i%20 == 1 && k == 1 {
			title = "changed"
		}
		email := ""
		if i%5 != 0 {
			email = fmt.Sprintf(`,"email":"user%d.%d@example.com","user%d@example.com":true`, i, k, k)
		}
		price := fmt.Sprintf("%d.%02d", k%1000, k%100)
		if cur && i%50 == 39 && k == 0 {
			price += "0"
		}
		tags := `["a","b","c` + fmt.Sprint(k%7) + `"]`
		if cur && i%50 == 29 && k == 1 {
			tags = `["a","b"]`
		}
		head := fmt.Sprintf(`"id":%d,"uuid":"%08x-1c2d-4e5f-8a9b-%012x"`, i*100000+k, k*2654435761%0xffffffff, i*1000+k)
		if cur && i%50 == 9 && k == 0 {
			head = fmt.Sprintf(`"uuid":"%08x-1c2d-4e5f-8a9b-%012x","id":%d`, k*2654435761%0xffffffff, i*1000+k, i*100000+k)
		}
		extra := ""
		if cur && i%50 == 19 && k == 1 {
			extra = `,"extra":null`
		}
		fmt.Fprintf(&b, `{%s,"title":"%s","slug":"item-%d-%d","price":%s,"active":%t,"created_at":"2026-01-%02dT08:%02d:00.000000Z","tags":%s,"author":{"id":%d,"name":"Author \u0160 %d \"q\""%s},"body":"%s"%s}`,
			head, title, i, k, price, k%3 == 0, k%28+1, k%60, tags, k%97, k%97, email,
			strings.Repeat("lorem ipsum dolor sit amet ", 1+k%6), extra)
	}
	b.WriteString("]}")
	if html {
		b.WriteString(`}</script></body></html>`)
	}
	return b.Bytes()
}

// perfAllow has the rules of a real project: thirty that name other
// requests, the time, rules for a few requests inside the data (one with
// parts, one any_length), and a format that a changed time breaks.
func perfAllow() string {
	var b strings.Builder
	b.WriteString("version: 1\nheaders:\n  - name: date\n    format: '.*'\n    reason: time\nbody:\n")
	b.WriteString("  - request: get-r-1?\n    path: $.meta.generated_at\n    format: '.*T10:.*'\n    reason: breaks on the new time\n")
	b.WriteString("  - path: $.meta.generated_at\n    format: '\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z'\n    reason: time\n")
	b.WriteString("  - path: html>body>script#__NEXT_DATA__$.props.meta.generated_at\n    format: '.+'\n    reason: time\n")
	b.WriteString("  - path: html>body>script#__NEXT_DATA__$.buildId\n    format: 'b\\d'\n    reason: build\n")
	b.WriteString("  - request: get-r-2*\n    path: $.data[*].author.id\n    format: '\\d+'\n    reason: r\n")
	b.WriteString("  - request: get-r-5*\n    path: $.data[2].author.id\n    format: '[a-z]+'\n    reason: breaks on an unchanged value\n")
	b.WriteString("  - request: get-r-3*\n    path: $.data[*].title\n    format: '\\d+'\n    parts: true\n    reason: r\n")
	b.WriteString("  - request: get-r-4*\n    path: $.data[*].tags\n    any_length: true\n    reason: r\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "  - request: get-x-%d*\n    path: $.data[*].field%d\n    format: '.*'\n    reason: r\n", i, i)
	}
	return b.String()
}

// perfProject writes a project with captures a and b of n requests, 80% of
// them private, and returns its configuration.
func perfProject(tb testing.TB, n, scale int) string {
	tb.Helper()
	root := tb.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	files := map[string]string{
		cfg: "version: 1\nproject: perf\nmode: freeze\nlevel: values\ntarget:\n  base_url: http://127.0.0.1:9\n",
		filepath.Join(root, "driftgate", "allowlist.yaml"): perfAllow(),
	}
	var cat strings.Builder
	cat.WriteString(`{"version":1,"requests":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			cat.WriteByte(',')
		}
		fmt.Fprintf(&cat, `{"id":"get-r-%d","method":"GET","path":"/r/%d","storage":"public"}`, i, i)
	}
	cat.WriteString("]}")
	files[filepath.Join(root, "driftgate", "catalog.json")] = cat.String()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	tb.Setenv("DRIFTGATE_STORE", filepath.Join(tb.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	tb.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	p, err := config.Load(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	for _, tag := range []string{"a", "b"} {
		set, err := p.Capture(tag)
		if err != nil {
			tb.Fatal(err)
		}
		cur := tag == "b"
		for i := 0; i < n; i++ {
			if cur && i == n-1 {
				continue // not captured
			}
			r := &snapshot.Record{ID: fmt.Sprintf("get-r-%d", i), Status: 200, Body: snapshot.Body{Storage: snapshot.Public}}
			ct := "application/json"
			if i%30 == 11 {
				ct = "text/html; charset=utf-8"
			}
			date := "Thu, 29 Oct 2026 10:00:00 GMT"
			if cur {
				date = "Thu, 29 Oct 2026 11:11:11 GMT"
			}
			r.Headers = [][2]string{{"cache-control", "no-cache, private"}, {"content-type", ct}, {"date", date}}
			if i%5 != 0 {
				r.Body.Storage = snapshot.Private
			}
			if cur && i%97 == 5 {
				r.Status, r.Error, r.Headers = 0, "timeout", nil
				if err := set.Write(r, []byte{}); err != nil {
					tb.Fatal(err)
				}
				continue
			}
			if err := set.Write(r, perfBody(i, scale, cur)); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return cfg
}

var createdField = regexp.MustCompile(`"created": *"[^"]*"`)

// compareReport runs compare b --against a and returns the JSON report it
// writes and the one it saves, without the time they were made.
func compareReport(tb testing.TB, cfg string, extra ...string) []byte {
	tb.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"compare", "b", "--against", "a", "--config", cfg, "--format", "json", "--max-drifts", "1000000"}, extra...)
	if code := Main(args, &out, &errOut); code != ExitDrift {
		tb.Fatalf("compare: exit %d\n%s", code, errOut.String())
	}
	saved, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "last-compare-*.json"))
	if len(saved) != 1 {
		tb.Fatalf("saved reports: %v", saved)
	}
	raw, err := os.ReadFile(saved[0])
	if err != nil {
		tb.Fatal(err)
	}
	out.WriteString("\n--- saved\n")
	out.Write(raw)
	return createdField.ReplaceAll(out.Bytes(), []byte(`"created": ""`))
}

// The fast paths and comparing requests in parallel change nothing: the
// reports are the same byte for byte, in catalog order, run after run.
func TestCompareFastAndParallel(t *testing.T) {
	cfg := perfProject(t, 600, 64)
	defer func(w int, b int64) { compareWorkers, compareBudget = w, b }(compareWorkers, compareBudget)
	defer func() { compare.FastPaths = true }()
	runs := []struct {
		name    string
		fast    bool
		workers int
		budget  int64
	}{
		{"serial", true, 1, 256 << 20},
		{"parallel without fast paths", false, 8, 256 << 20},
		{"parallel", true, 8, 256 << 20},
		{"small budget", true, 8, 64 << 10},
		{"one per CPU", true, 0, 256 << 20},
		{"one per CPU again", true, 0, 256 << 20},
	}
	for _, extra := range [][]string{nil, {"--show-private"}} {
		compare.FastPaths, compareWorkers, compareBudget = false, 1, 256<<20
		want := compareReport(t, cfg, extra...)
		for _, kind := range []string{"value", "format", "key-order", "key-extra", "array-length", "number-literal", "status", "not-captured"} {
			if !bytes.Contains(want, []byte(`"kind": "`+kind+`"`)) {
				t.Fatalf("no %s drift in the report", kind)
			}
		}
		for _, r := range runs {
			compare.FastPaths, compareWorkers, compareBudget = r.fast, r.workers, r.budget
			if got := compareReport(t, cfg, extra...); !bytes.Equal(got, want) {
				t.Errorf("%v %s: the report differs from the serial one without fast paths", extra, r.name)
			}
		}
	}
}

// BenchmarkCompare compares two captures of 600 requests, about 95 MB on
// each side with bodies of up to 20 MB, 80% of them private.
func BenchmarkCompare(b *testing.B) {
	cfg := perfProject(b, 600, 1)
	b.ResetTimer()
	for b.Loop() {
		compareReport(b, cfg)
	}
}

// sameBodyProject is perfProject with the first n requests answered by the
// same body of size bytes in both captures, public and private ones, and
// returns the configuration and those bodies' files in the stores.
func sameBodyProject(t *testing.T, n, size int) (string, []string) {
	cfg := perfProject(t, n+5, 64)
	p, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, tag := range []string{"a", "b"} {
		set, err := p.Capture(tag)
		if err != nil {
			t.Fatal(err)
		}
		for i := range n {
			body := bytes.Repeat([]byte(fmt.Sprintf(`{"i":%d}`, i)), size/8)
			r := &snapshot.Record{ID: fmt.Sprintf("get-r-%d", i), Status: 200, Headers: [][2]string{{"content-type", "application/json"}},
				Body: snapshot.Body{Storage: snapshot.Public}}
			if i%2 == 1 {
				r.Body.Storage = snapshot.Private
			}
			if err := set.Write(r, body); err != nil {
				t.Fatal(err)
			}
			if tag == "b" {
				if r.Body.Storage == snapshot.Private {
					files = append(files, filepath.Join(set.Private.Dir, r.Body.Ref+set.Private.Ext))
				} else {
					files = append(files, filepath.Join(set.Dir, "bodies", r.Body.SHA256))
				}
			}
		}
	}
	return cfg, files
}

// Two bodies with the same name (SHA-256 or ref) are the same bytes:
// compare reads neither. Damaged files show it: they would not match their
// names if they were read, as they are without the fast paths.
func TestCompareDoesNotReadEqualBodies(t *testing.T) {
	// Larger than the other private bodies: the key check reads the smallest.
	cfg, files := sameBodyProject(t, 4, 4<<10)
	want := compareReport(t, cfg)
	for _, f := range files {
		if err := os.WriteFile(f, []byte("damaged"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := compareReport(t, cfg); !bytes.Equal(got, want) {
		t.Fatal("the report changed when bodies with the same name were damaged")
	}
	defer func() { compare.FastPaths = true }()
	compare.FastPaths = false
	var out, errOut bytes.Buffer
	if code := Main([]string{"compare", "b", "--against", "a", "--config", cfg}, &out, &errOut); code != ExitEnv {
		t.Fatalf("without the fast paths: exit %d, want %d (the damaged bodies are read)\n%s", code, ExitEnv, errOut.String())
	}
}

// Comparing captures whose bodies did not change takes memory for the
// records only, not for the bodies (a memory regression check).
func TestCompareMemoryOfEqualBodies(t *testing.T) {
	const n, size = 40, 512 << 10
	cfg, _ := sameBodyProject(t, n, size)
	compareReport(t, cfg) // the first run reads what is read once
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	compareReport(t, cfg)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > n*size/3 {
		t.Fatalf("%d bytes allocated to compare %d unchanged bodies of %d bytes", got, n, size)
	}
}
