package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

func TestInitSuggestsPageRules(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"next.config.mjs", "self.__next_f$.0.b"},
		{"nuxt.config.ts", `buildId:"[\w-]+"`},
	} {
		root := t.TempDir()
		write(t, filepath.Join(root, c.file), "export default {}\n")
		if code, out, errOut := runCLI(t, "init", "--config", filepath.Join(root, "driftgate.yaml")); code != 0 {
			t.Fatalf("init: %d %s %s", code, out, errOut)
		}
		raw, _ := os.ReadFile(filepath.Join(root, "driftgate", "allowlist.yaml"))
		if !strings.Contains(string(raw), c.want) || !strings.Contains(string(raw), "parts: true") {
			t.Errorf("%s: allowlist lacks the page rules:\n%s", c.file, raw)
		}
		if _, err := allowlist.Load(filepath.Join(root, "driftgate", "allowlist.yaml")); err != nil {
			t.Errorf("%s: %v", c.file, err)
		}
	}
}

// nextBuild serves a page the way next start does: the build id and the
// hashes of the scripts change with every build, the content with the code.
type nextBuild struct {
	mu                 sync.Mutex
	id, hash, title, n string
}

func (b *nextBuild) set(id, hash, title, n string) {
	b.mu.Lock()
	b.id, b.hash, b.title, b.n = id, hash, title, n
	b.mu.Unlock()
}

func (b *nextBuild) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(b.id+b.hash+b.title+b.n)))[:15]+`"`)
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><script src="/_next/static/chunks/%s.js" defer=""></script><script src="/_next/static/%s/_buildManifest.js" defer=""></script></head>`+
		`<body><div id="__next"><main id="app"><h1 class="title">%s</h1><ul class="items"><li data-id="1">Apple</li><li data-id="2">Pear</li></ul></main></div>`+
		`<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"%s","n":%s}},"page":"/","buildId":"%s"}</script>`+
		`<script>self.__next_f.push([1,"0:{\"P\":null,\"b\":\"%s\"}\n1:I[123,[\"/_next/static/chunks/%s.js\"],\"default\"]\n"])</script></body></html>`,
		b.hash, b.id, b.title, b.title, b.n, b.id, b.id, b.hash)
}

// TestPagesAcrossBuilds: with the rules init suggests for Next.js, two
// builds of the same page compare clean, and a change of text, attribute or
// __NEXT_DATA__ value is reported at its path.
func TestPagesAcrossBuilds(t *testing.T) {
	app := &nextBuild{}
	app.set("uAe4HU0mNj2XisFi1omht", "0cz1d0mv5g_q7", "Products", "1")
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	write(t, filepath.Join(root, "next.config.mjs"), "export default {}\n")
	cfg := filepath.Join(root, "driftgate.yaml")
	if code, out, errOut := runCLI(t, "init", "--config", cfg); code != 0 {
		t.Fatalf("init: %d %s %s", code, out, errOut)
	}
	write(t, filepath.Join(root, "routes-src.json"), `[{"method":"GET","path":"/"}]`)
	write(t, cfg, fmt.Sprintf("version: 1\nproject: web\ntarget:\n  base_url: %s\ndiscover:\n  - adapter: command\n    command: [\"cat\", \"routes-src.json\"]\n", srv.URL))
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	if code, out, errOut := runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	app.set("n8OdbNLqalHd8DOm3x6bM", "1gc05-w8w6van", "Products", "1")
	if code, out, _ := runCLI(t, "run", "--config", cfg); code != ExitClean || !strings.Contains(out, "No drift") {
		t.Fatalf("another build: %d\n%s", code, out)
	}
	for _, c := range []struct{ title, n, want string }{
		{"Goods", "1", "text            html>body>div#__next>main#app>h1"},
		{"Products", "2", "value           html>body>script#__NEXT_DATA__$.props.pageProps.n"},
	} {
		app.set("x8OdbNLqalHd8DOm3x6bM", "2gc05-w8w6van", c.title, c.n)
		if code, out, _ := runCLI(t, "run", "--config", cfg); code != ExitDrift || !strings.Contains(out, c.want) {
			t.Fatalf("want %s: %d\n%s", c.want, code, out)
		}
	}
}

func pageDrifts(t *testing.T, rules, base, cur string) []compare.Drift {
	t.Helper()
	l, err := allowlist.Parse([]byte("version: 1\nbody:\n" + rules))
	if err != nil {
		t.Fatal(err)
	}
	side := func(body string) compare.Side {
		return compare.Side{
			Record: &snapshot.Record{Status: 200, Headers: [][2]string{{"Content-Type", "text/html"}}, Body: snapshot.Body{SHA256: snapshot.Sum([]byte(body))}},
			Body:   []byte(body),
		}
	}
	return compare.One("page", side(base), side(cur), l, compare.LevelValues)
}

func reverse(s string) string {
	r := []byte(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// TestInitAssetRules: the asset rules init suggests let the hashes in real
// Next.js and Nuxt pages change, and nothing else: no . or .. segment, no
// second asset URL run on after the first.
func TestInitAssetRules(t *testing.T) {
	dir := filepath.Join("..", "compare", "testdata", "html")
	rebuild := func(file, stems string, ids ...string) (string, string) {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		cur := regexp.MustCompile(stems).ReplaceAllStringFunc(page, func(m string) string {
			i := strings.LastIndexByte(m, '/') + 1
			return m[:i] + reverse(m[i:])
		})
		for i := 0; i+1 < len(ids); i += 2 {
			cur = strings.ReplaceAll(cur, ids[i], ids[i+1])
		}
		if cur == page {
			t.Fatalf("%s: nothing changed", file)
		}
		return page, cur
	}
	for _, c := range []struct {
		file, rules string
		base, cur   string
	}{
		{file: "next-app-router.html", rules: nextAllow()},
		{file: "next-pages-router.html", rules: nextAllow()},
		{file: "nuxt.html", rules: nuxtAllow()},
	} {
		var base, cur string
		switch c.file {
		case "nuxt.html":
			base, cur = rebuild(c.file, `/_nuxt/[\w-]+`, "86d14541-27c3-4c3a-9502-49ec82b87fef", "0f7e3c1a-1b2c-4d5e-8f90-a1b2c3d4e5f6")
		default:
			base, cur = rebuild(c.file, `static/(?:chunks|media)/[\w-]+`, "uAe4HU0mNj2XisFi1omht", "n8OdbNLqalHd8DOm3x6bM")
		}
		if ds := pageDrifts(t, c.rules, base, cur); len(ds) != 0 {
			t.Errorf("%s: another build reported: %+v", c.file, ds)
		}
	}
	for _, c := range []struct{ rules, base, cur string }{
		{nextAllow(), `<script src="/_next/static/chunks/main-abc.js"></script>`, `<script src="/_next/static/../../uploads/evil.js"></script>`},
		{nextAllow(), `<script src="/_next/static/chunks/main-abc.js"></script>`, `<script src="/_next/static/chunks/../main-abc.js"></script>`},
		{nextAllow(), `<script src="/_next/static/chunks/main-abc.js"></script>`, `<script src="/_next/static/./chunks/main-abc.js"></script>`},
		{nextAllow(), `<a href="/_next/static/a.js">x</a>`, `<a href="/_next/static/a.js/_next/static/b.js">x</a>`},
		{nextAllow(), `<a href="/_next/static/chunks/a.js">x</a>`, `<a href="/_next/static/chunks/_next/static/b.js">x</a>`},
		{nextAllow(), `<img src="/_next/image?url=%2F_next%2Fstatic%2Fmedia%2Fa.png">`, `<img src="/_next/image?url=%2F_next%2Fstatic%2F..%2F..%2Fa.png">`},
		{nextAllow(), `<img src="/_next/image?url=%2F_next%2Fstatic%2Fmedia%2Fa.png">`, `<img src="/_next/image?url=%2F_next%2Fstatic%2F%2E%2E%2Fa.png">`},
		{nextAllow(), `<img src="/_next/image?url=%2F_next%2Fstatic%2Fmedia%2Fa.png">`, `<img src="/_next/image?url=%2F_next%2Fstatic%2Fmedia%2F_next%2Fstatic%2Fb.png">`},
		{nextAllow(), `<p>static/chunks/a.js</p>`, `<p>static/chunks/x/static/chunks/b.js</p>`},
		{nuxtAllow(), `<script src="/_nuxt/BS7A41d5.js"></script>`, `<script src="/_nuxt/../uploads/evil.js"></script>`},
		{nuxtAllow(), `<script src="/_nuxt/BS7A41d5.js"></script>`, `<script src="/_nuxt/a/_nuxt/b.js"></script>`},
	} {
		if ds := pageDrifts(t, c.rules, c.base, c.cur); len(ds) == 0 {
			t.Errorf("%s -> %s: not reported", c.base, c.cur)
		}
	}
	// What a build changes: hashes, nested directories, the build id.
	for _, c := range []struct{ rules, base, cur string }{
		{nextAllow(), `<a href="/_next/static/chunks/app/_not-found/page-0a1b.js">x</a>`, `<a href="/_next/static/chunks/app/_not-found/page-9f8e.js">x</a>`},
		{nextAllow(), `<a href="/_next/static/b1/_buildManifest.js">x</a>`, `<a href="/_next/static/b2/_buildManifest.js">x</a>`},
		{nextAllow(), `<img src="/_next/image?url=%2F_next%2Fstatic%2Fmedia%2Fa.1234.png&amp;w=64">`, `<img src="/_next/image?url=%2F_next%2Fstatic%2Fmedia%2Fa.5678.png&amp;w=64">`},
		{nuxtAllow(), `<link href="/_nuxt/builds/meta/86d1.json">`, `<link href="/_nuxt/builds/meta/0f7e.json">`},
	} {
		if ds := pageDrifts(t, c.rules, c.base, c.cur); len(ds) != 0 {
			t.Errorf("%s -> %s: %+v", c.base, c.cur, ds)
		}
	}
}
