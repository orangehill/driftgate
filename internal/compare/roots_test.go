package compare

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

const (
	baseRoot = "/Users/dev/Sites/shop"
	curRoot  = "/tmp/wt/shop"
)

// rooted compares a body captured in baseRoot with one captured in
// curRoot (with its real path /private/tmp/wt/shop); {r} in either stands
// for the side's folder.
func rooted(t *testing.T, headers [][2]string, base, cur string) []Drift {
	t.Helper()
	b := side(200, headers, strings.ReplaceAll(base, "{r}", baseRoot))
	c := side(200, headers, strings.ReplaceAll(cur, "{r}", curRoot))
	b.Roots, c.Roots = []string{baseRoot}, []string{curRoot, "/private" + curRoot}
	return both(t, nil, b, c)
}

// TestRootsDebugTrace: a stack trace of the application, written in the
// two checkouts' folders, is the same; a change of file, line or folder
// is still a drift.
func TestRootsDebugTrace(t *testing.T) {
	trace := `{"message":"Server Error","exception":"ErrorException","file":"{r}/app/Http/Controllers/NewsController.php","line":12,` +
		`"trace":[{"file":"{r}\/vendor\/laravel\/framework\/src\/Illuminate\/Routing\/Controller.php","line":54}]}`
	if ds := rooted(t, jsonType, trace, trace); len(ds) != 0 {
		t.Errorf("the same trace in two folders: %+v", ds)
	}
	real := strings.ReplaceAll(trace, "{r}", "/private{r}")
	if ds := rooted(t, jsonType, trace, real); len(ds) != 0 {
		t.Errorf("the real path of the folder: %+v", ds)
	}
	html := [][2]string{{"content-type", "text/html"}}
	if ds := rooted(t, html, "<p>{r}/app/Models/User.php:31</p>", "<p>{r}/app/Models/User.php:31</p>"); len(ds) != 0 {
		t.Errorf("a debug page: %+v", ds)
	}
	for _, c := range []struct{ name, base, cur string }{
		{"line", `{"file":"{r}/app/A.php","line":12}`, `{"file":"{r}/app/A.php","line":13}`},
		{"file", `{"file":"{r}/app/A.php"}`, `{"file":"{r}/app/B.php"}`},
		{"another folder", `{"file":"{r}/app/A.php"}`, `{"file":"{r}2/app/A.php"}`},
	} {
		if ds := rooted(t, jsonType, c.base, c.cur); len(ds) != 1 {
			t.Errorf("%s: %+v", c.name, ds)
		}
	}
	// Captured in one folder, or without a record of it, the bodies are
	// compared as they are.
	b := side(200, jsonType, `{"file":"`+baseRoot+`/a.php"}`)
	c := side(200, jsonType, `{"file":"`+curRoot+`/a.php"}`)
	b.Roots = []string{baseRoot}
	if ds := both(t, nil, b, c); len(ds) != 1 {
		t.Errorf("one side without its folder: %+v", ds)
	}
}

// TestRootsContentLength: bodies equal but for folders of other lengths
// differ in Content-Length only, which is no drift.
func TestRootsContentLength(t *testing.T) {
	body := func(root string) string { return `{"file":"` + root + `/app/A.php"}` }
	b := side(200, [][2]string{{"content-type", "application/json"}, {"content-length", fmt.Sprint(len(body(baseRoot)))}}, body(baseRoot))
	c := side(200, [][2]string{{"content-type", "application/json"}, {"content-length", fmt.Sprint(len(body(curRoot)))}}, body(curRoot))
	b.Roots, c.Roots = []string{baseRoot}, []string{curRoot}
	if ds := both(t, nil, b, c); len(ds) != 0 {
		t.Errorf("content-length of bodies that differ only in the folders: %+v", ds)
	}
}

// TestRootsBoundaries: a folder of one name (/app) is not replaced, as it
// is also a URL's path; nor one inside another path; a body that names
// the other side's folder is the same on both sides.
func TestRootsBoundaries(t *testing.T) {
	forms := rootForms([]string{"/app", "/srv/app"})
	if got := string(replaceRoots([]byte(`"/app/settings" "/srv/app/x" "/data/srv/app/y" "http://h/srv/app/z"`), forms)); got != `"/app/settings" "/driftgate-project/x" "/data/srv/app/y" "http://h/srv/app/z"` {
		t.Errorf("boundaries: %s", got)
	}
	same := `{"main":"` + baseRoot + `/a.php"}`
	b, c := side(200, jsonType, same), side(200, jsonType, same)
	b.Roots, c.Roots = []string{baseRoot}, []string{curRoot}
	if ds := both(t, nil, b, c); len(ds) != 0 {
		t.Errorf("a body that names one side's folder on both: %+v", ds)
	}
}

func TestReplaceRootsWindows(t *testing.T) {
	forms := rootForms([]string{`C:\src\shop`})
	got := string(replaceRoots([]byte(`{"file":"C:\\src\\shop\\app\\A.php","x":"C:\\src\\shopping"}`), forms))
	if got != `{"file":"\\driftgate-project\\app\\A.php","x":"C:\\src\\shopping"}` {
		t.Errorf("Windows path in JSON: %s", got)
	}
}

// TestRootsInHome: a folder recorded as ~/… is that path in any home
// folder: two users' checkouts, and one against CI's folder.
func TestRootsInHome(t *testing.T) {
	trace := func(root string) string {
		return `{"file":"` + root + `/app/A.php","trace":[{"file":"` + strings.ReplaceAll(root, "/", `\/`) + `\/vendor\/x.php"}]}`
	}
	for _, c := range []struct{ name, base, cur, baseBody, curBody string }{
		{"two users", "~/Sites/shop", "~/Sites/shop", trace("/Users/alice/Sites/shop"), trace("/home/bob/Sites/shop")},
		{"against CI", "~/Sites/shop", "/home/runner/work/shop/shop", trace("/Users/alice/Sites/shop"), trace("/home/runner/work/shop/shop")},
		{"root's home", "~/src/shop", "/srv/shop", trace("/root/src/shop"), trace("/srv/shop")},
	} {
		b, cs := side(200, jsonType, c.baseBody), side(200, jsonType, c.curBody)
		b.Roots, cs.Roots = []string{c.base}, []string{c.cur}
		if ds := both(t, nil, b, cs); len(ds) != 0 {
			t.Errorf("%s: %+v", c.name, ds)
		}
	}
	// Outside a home folder, or another project there, it still differs.
	for _, body := range []string{trace("/opt/alice/Sites/shop"), trace("/Users/alice/Sites/shop2")} {
		b, cs := side(200, jsonType, trace("/Users/alice/Sites/shop")), side(200, jsonType, body)
		b.Roots, cs.Roots = []string{"~/Sites/shop"}, []string{"/srv/other"}
		if ds := both(t, nil, b, cs); len(ds) == 0 {
			t.Errorf("%s taken for the project's folder", body)
		}
	}
	// This machine's home of another form, a person's: a capture there
	// recorded ~/…, and the comparison here takes that home for one.
	if runtime.GOOS != "windows" {
		t.Setenv("HOME", "/nfs/home/jane")
		b, cs := side(200, jsonType, trace("/Users/alice/Sites/shop")), side(200, jsonType, trace("/nfs/home/jane/wt/shop"))
		b.Roots, cs.Roots = []string{"~/Sites/shop"}, []string{"~/wt/shop"}
		if ds := both(t, nil, b, cs); len(ds) != 0 {
			t.Errorf("a home of another form: %+v", ds)
		}
	}
	forms := rootForms([]string{`~\src\shop`})
	if got := string(replaceRoots([]byte(`{"f":"C:\\Users\\alice\\src\\shop\\A.php"}`), forms)); got != `{"f":"\\driftgate-project\\A.php"}` {
		t.Errorf("Windows home in JSON: %s", got)
	}
}

// TestHomeRelative: the folders recorded as ~ and the rest, WSL's too;
// the home folder itself and one name in it are not recorded, and a ~ or
// ~/<one name> record, as an earlier release could write one, is left
// out: /home/welcome and /home/dashboard (or a URL's /home/x) stay
// different.
func TestHomeRelative(t *testing.T) {
	for folder, want := range map[string][2]any{
		"/Users/alice/Sites/shop": {"/Sites/shop", true}, "/home/bob/a/b": {"/a/b", true}, "/root/src/shop": {"/src/shop", true},
		"/mnt/c/Users/alice/src/shop": {"/src/shop", true}, `C:\Users\alice\src\shop`: {`\src\shop`, true},
		"/Users/alice": {"", true}, "/home/bob/app": {"", true},
		"/var/lib/jenkins/jobs/shop": {"", false}, "/home/users/bob/shop": {"/bob/shop", true}, "/srv/shop": {"", false},
		`C:\Users\John Doe\src\shop`: {"", false},
	} {
		rest, home := HomeRelative(folder)
		if rest != want[0] || home != want[1] {
			t.Errorf("HomeRelative(%s) = %q %v, want %v", folder, rest, home, want)
		}
	}
	b, c := side(200, jsonType, `{"f":"/mnt/c/Users/alice/src/shop/a.php"}`), side(200, jsonType, `{"f":"/Users/bob/src/shop/a.php"}`)
	b.Roots, c.Roots = []string{"~/src/shop"}, []string{"~/src/shop"}
	if ds := both(t, nil, b, c); len(ds) != 0 {
		t.Errorf("WSL against macOS: %+v", ds)
	}
	for _, roots := range [][]string{{"~"}, {"~/app"}} {
		for _, pair := range [][2]string{{`{"to":"/home/welcome"}`, `{"to":"/home/dashboard"}`}, {`{"to":"\/home\/welcome"}`, `{"to":"\/home\/dashboard"}`},
			{`{"to":"/root"}`, `{"to":"/home/x"}`}, {`{"to":"/home/x/app"}`, `{"to":"/home/y/app"}`}} {
			b, c := side(200, jsonType, pair[0]), side(200, jsonType, pair[1])
			b.Roots, c.Roots = roots, []string{"/srv/shop"}
			if ds := both(t, nil, b, c); len(ds) == 0 {
				t.Errorf("%v: %s and %s taken for the same", roots, pair[0], pair[1])
			}
		}
	}
}

// BenchmarkRoots: a comparison of two equal bodies and of two that differ
// in one value (about 20 kB of JSON, with or without 200 paths of the
// project's folder, as a stack trace has), with no record of the folders
// ("none", as before the records), with one folder in a home folder on
// both sides ("home": the folders are replaced), and with the folders'
// forms made once for every request ("home-once", as the commands do).
func BenchmarkRoots(b *testing.B) {
	for _, paths := range []bool{false, true} {
		var items []string
		for i := range 200 {
			file := fmt.Sprintf("app/N%d.php", i)
			if paths {
				file = "/Users/alice/Sites/shop/" + file
			}
			items = append(items, fmt.Sprintf(`{"id":%d,"title":"news item %d","file":%q}`, i, i, file))
		}
		body := `{"data":[` + strings.Join(items, ",") + `]}`
		changed := strings.Replace(body, `"title":"news item 7"`, `"title":"news item 7!"`, 1)
		kind := map[bool]string{false: "plain", true: "paths"}[paths]
		benchRoots(b, kind, body, changed)
	}
}

func benchRoots(b *testing.B, kind, body, changed string) {
	for _, c := range []struct{ name, cur string }{{"equal", body}, {"one-value", changed}} {
		for _, mode := range []string{"none", "home", "home-once"} {
			b.Run(kind+"/"+c.name+"/"+mode, func(b *testing.B) {
				base, cur := side(200, jsonType, body), side(200, jsonType, c.cur)
				if mode != "none" {
					base.Roots, cur.Roots = []string{"~/Sites/shop"}, []string{"~/Sites/shop"}
				}
				if mode == "home-once" {
					f := NewFolders(base.Roots, cur.Roots)
					base.Folders, cur.Folders = f, f
				}
				allow := allowlist.Empty()
				for b.Loop() {
					One("x", base, cur, allow, LevelValues)
				}
			})
		}
	}
}

// TestRootsNoFalsePass: what the replacement of the folders must never
// take for the same (the cases of an independent check): another folder
// or file, a folder's name in a URL or a text, a home folder after a byte
// a path may hold (+), one past the look-back, and nested folders of the
// two sides; a header and an error that differ still drift.
func TestRootsNoFalsePass(t *testing.T) {
	long := strings.Repeat("n", 249)
	for _, c := range []struct {
		name           string
		broots, croots []string
		bbody, cbody   string
	}{
		{"prefix of the folder", []string{"/w/main/shop"}, []string{"/w/feat/shop"}, `{"f":"/w/main/x"}`, `{"f":"/w/feat/x"}`},
		{"the folder in a URL", []string{"/w/main/shop"}, []string{"/w/feat/shop"}, `{"u":"http://h/w/main/shop/x"}`, `{"u":"http://h/w/feat/shop/x"}`},
		{"names in a text", []string{"/w/main/shop"}, []string{"/w/feat/shop"}, `{"t":"main shop"}`, `{"t":"feat shop"}`},
		{"~ in an absolute URL", []string{"~/a/b"}, []string{"~/a/b"}, `{"u":"https://e.com/home/x/a/b/c"}`, `{"u":"https://e.com/home/y/a/b/c"}`},
		{"escaped, another file", []string{"/w/main/shop"}, []string{"/w/feat/shop"}, `{"f":"\/w\/main\/shop\/a"}`, `{"f":"\/w\/feat\/shop\/b"}`},
		{"doubled, another file", []string{`C:\w\main`}, []string{`C:\w\feat`}, `{"f":"C:\\w\\main\\a"}`, `{"f":"C:\\w\\feat\\b"}`},
		{"drive in another case", []string{`C:\w\main`}, []string{`C:\w\feat`}, `{"f":"c:\\w\\main\\a"}`, `{"f":"C:\\w\\feat\\a"}`},
		{"regexp characters", []string{"/w/a.b+(c)/s"}, []string{"/w/feat/s"}, `{"f":"/w/aXb+(c)/s/a"}`, `{"f":"/w/feat/s/a"}`},
		{"regexp characters in a home", []string{"~/a.b/s(x)"}, []string{"/w/feat/s"}, `{"f":"/home/u/aXb/s(x)/a"}`, `{"f":"/w/feat/s/a"}`},
		{"another folder of the length", []string{"/w/main/shop"}, []string{"/w/feat/shop"}, `{"f":"/w/main/shop/a"}`, `{"f":"/w/xxxx/shop/a"}`},
		{"a home past the look-back", []string{"~/S/shop"}, []string{"/srv/x/y"}, `{"f":"x:/home/` + long + `a/S/shop/a"}`, `{"f":"x:/home/` + long + `b/S/shop/a"}`},
		{"a home after +", []string{"~/S/shop"}, []string{"/srv/x/y"}, `{"f":"/data+/home/a/S/shop/a"}`, `{"f":"/data+/home/b/S/shop/a"}`},
		{"nested folders of the sides", []string{"/var/www/html"}, []string{"/var/www"}, `{"f":"/var/www/x"}`, `{"f":"/var/www/html/x"}`},
		{"no folder recorded", nil, []string{"/w/feat/shop"}, `{"f":"/w/main/shop/a"}`, `{"f":"/w/feat/shop/a"}`},
		{"home, escaped after a plain home", []string{"~/S/shop"}, []string{"~/S/shop"}, `{"f":"/Users/a\/S\/shop\/x"}`, `{"f":"/Users/b\/S\/shop\/x"}`},
		{"home, another file", []string{"~/S/shop"}, []string{"~/S/shop"}, `{"f":"/Users/a/S/shop/x"}`, `{"f":"/Users/b/S/shop/y"}`},
	} {
		b, cs := side(200, jsonType, c.bbody), side(200, jsonType, c.cbody)
		b.Roots, cs.Roots = c.broots, c.croots
		if ds := both(t, nil, b, cs); len(ds) == 0 {
			t.Errorf("%s: no drift", c.name)
		}
	}
	b := side(302, [][2]string{{"location", "/w/main/shop/a"}}, "")
	cs := side(302, [][2]string{{"location", "/w/feat/shop/b"}}, "")
	b.Roots, cs.Roots = []string{"/w/main/shop"}, []string{"/w/feat/shop"}
	if ds := both(t, nil, b, cs); len(ds) == 0 {
		t.Error("Location to another file: no drift")
	}
	b, cs = side(200, nil, ""), side(200, nil, "")
	b.Record.Error, cs.Record.Error = "dial /w/main/shop/sock: refused", "dial /w/feat/shop/sock2: refused"
	b.Roots, cs.Roots = []string{"/w/main/shop"}, []string{"/w/feat/shop"}
	if ds := both(t, nil, b, cs); len(ds) == 0 {
		t.Error("errors of other files: no drift")
	}
	body := `{"f":"/w/main/shop/a"}`
	b = side(200, [][2]string{{"content-type", "application/json"}, {"x-path", "/w/main/shop/a"}}, body)
	cs = side(200, [][2]string{{"content-type", "application/json"}, {"x-path", "/w/feat/shop/zz"}}, body)
	b.Roots, cs.Roots = []string{"/w/main/shop"}, []string{"/w/feat/shop"}
	if ds := both(t, nil, b, cs); len(ds) == 0 {
		t.Error("a header of another file: no drift")
	}
	b = side(200, [][2]string{{"content-type", "application/json"}, {"content-length", "22"}}, body)
	cs = side(200, [][2]string{{"content-type", "application/json"}, {"content-length", "99"}}, body)
	b.Roots, cs.Roots = []string{"/w/main/shop"}, []string{"/w/feat/shop"}
	if ds := both(t, nil, b, cs); len(ds) == 0 {
		t.Error("Content-Length of equal bodies: no drift")
	}
}

// TestRootsLimits: what the replacement takes for the project's folder
// though it may not be, as documented: a relative URL path of the form of
// a home folder and the project's names, and a body that holds the
// neutral folder itself.
func TestRootsLimits(t *testing.T) {
	for _, c := range []struct {
		broots, croots []string
		bbody, cbody   string
	}{
		{[]string{"~/a/b"}, []string{"~/a/b"}, `{"u":"/home/x/a/b/c"}`, `{"u":"/home/y/a/b/c"}`},
		{[]string{"/w/main/shop"}, []string{"/w/feat/shop"}, `{"f":"/w/main/shop/a"}`, `{"f":"/driftgate-project/a"}`},
	} {
		b, cs := side(200, jsonType, c.bbody), side(200, jsonType, c.cbody)
		b.Roots, cs.Roots = c.broots, c.croots
		if ds := both(t, nil, b, cs); len(ds) != 0 {
			t.Errorf("%s and %s: the documented limit changed: %+v", c.bbody, c.cbody, ds)
		}
	}
}

// TestHomeInAnyCase: a folder opened as /users/alice (macOS and Windows
// ignore case) is recorded as ~/…, and found in any case.
func TestHomeInAnyCase(t *testing.T) {
	for folder, want := range map[string]string{
		"/users/alice/Sites/shop": "/Sites/shop", "/HOME/bob/a/b": "/a/b", `c:\users\alice\src\shop`: `\src\shop`,
	} {
		if rest, home := HomeRelative(folder); rest != want || !home {
			t.Errorf("HomeRelative(%s) = %q %v", folder, rest, home)
		}
	}
	b, cs := side(200, jsonType, `{"f":"/users/alice/Sites/shop/a.php"}`), side(200, jsonType, `{"f":"/Users/bob/Sites/shop/a.php"}`)
	b.Roots, cs.Roots = []string{"~/Sites/shop"}, []string{"~/Sites/shop"}
	if ds := both(t, nil, b, cs); len(ds) != 0 {
		t.Errorf("a home in another case: %+v", ds)
	}
}

// TestLocalHomes: a person's home of another form, and the real path of
// a home of a known form, are taken for home folders; a service
// account's home and a known form itself are not.
func TestLocalHomes(t *testing.T) {
	for _, c := range []struct {
		home, real string
		want       []string
	}{
		{"/home/jane", "/home/jane", nil},
		{"/home/jane", "/nfs/home/jane", []string{"/nfs/home/jane"}},
		{"/home/jane", "/var/home/jane", []string{"/var/home/jane"}},
		{"/nfs/home/jane", "/nfs/home/jane", []string{"/nfs/home/jane"}},
		{"/var/lib/jenkins", "/var/lib/jenkins", nil},
		{"/srv/app", "/srv/app", nil},
		{"/data/home/Jane Doe", "/data/home/Jane Doe", nil},
	} {
		if got := localHomesOf(c.home, c.real); !slices.Equal(got, c.want) {
			t.Errorf("%s (%s): %q, want %q", c.home, c.real, got, c.want)
		}
	}
	// A letter of another byte length without case (İ, K) is a miss.
	if _, ok := inFolder(`D:\ilker\x`, `D:\İlker`); ok && runtime.GOOS == "linux" {
		t.Error("İ taken for i on Linux")
	}
}
