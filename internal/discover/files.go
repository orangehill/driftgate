package discover

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/routes"
)

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// firstDir returns the first of the folders that exists under root.
func firstDir(root string, names ...string) string {
	for _, n := range names {
		if p := filepath.Join(root, n); isDir(p) {
			return p
		}
	}
	return ""
}

// relSlash is dir relative to root, slash-separated and ending in a slash.
func relSlash(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel) + "/"
}

// walkFiles lists files under dir (slash-separated, relative), skipping
// dependencies and build output.
func walkFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".next", ".nuxt", ".output", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

var (
	scriptExt = lazy.Regexp(`\.(js|jsx|ts|tsx|mjs|cjs|mdx)$`)
	testFile  = lazy.Regexp(`\.(test|spec)\.|\.d\.ts$`)
	// Next.js and Nuxt write dynamic segments the same way:
	// [id], [...slug] (rest of the path), [[...slug]] (rest or nothing);
	// Nuxt also [[id]] (optional) and mixed segments like user-[id].
	unnamedCatchAll  = lazy.Regexp(`\[\.\.\.\]`) // Nuxt: [...], available as params._
	optionalCatchAll = lazy.Regexp(`\[\[\.\.\.([^\]]+)\]\]`)
	catchAll         = lazy.Regexp(`\[\.\.\.([^\]]+)\]`)
	optionalParam    = lazy.Regexp(`\[\[([^\]]+)\]\]`)
	param            = lazy.Regexp(`\[([^\]]+)\]`)
)

func segmentToPath(seg string) string {
	seg = unnamedCatchAll().ReplaceAllString(seg, "{_*}")
	seg = optionalCatchAll().ReplaceAllString(seg, "{${1}*?}")
	seg = catchAll().ReplaceAllString(seg, "{${1}*}")
	seg = optionalParam().ReplaceAllString(seg, "{${1}?}")
	return param().ReplaceAllString(seg, "{${1}}")
}

// urlPath turns folder segments into a URL path. Route groups "(name)" and
// Next.js parallel-route slots "@name" do not appear in the URL; a file
// named index stands for its folder (a folder named index does not).
func urlPath(segs []string, lastIsFile bool) string {
	var parts []string
	for i, s := range segs {
		if s == "" || (s == "index" && lastIsFile && i == len(segs)-1) || (strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")")) || strings.HasPrefix(s, "@") {
			continue
		}
		parts = append(parts, segmentToPath(s))
	}
	return "/" + strings.Join(parts, "/")
}

// hidden reports a folder that keeps files out of routing: Next.js private
// folders (_name) and intercepting routes ((.)name, (..)name, (...)name).
func hidden(segs []string) bool {
	for _, s := range segs {
		if strings.HasPrefix(s, "_") || strings.HasPrefix(s, "(.") {
			return true
		}
	}
	return false
}

var (
	exportFunc  = lazy.Regexp(`export\s+(?:async\s+)?function\s+(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)\b`)
	exportConst = lazy.Regexp(`export\s+(?:const|let|var)\s+(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)\b`)
	exportList  = lazy.Regexp(`export\s*\{([^}]*)\}`)
	methodName  = lazy.Regexp(`\b(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)\s*$`)
)

// routeMethods reads the HTTP methods a Next.js route handler exports.
func routeMethods(src string) []string {
	var ms []string
	for _, m := range exportFunc().FindAllStringSubmatch(src, -1) {
		ms = append(ms, m[1])
	}
	for _, m := range exportConst().FindAllStringSubmatch(src, -1) {
		ms = append(ms, m[1])
	}
	for _, m := range exportList().FindAllStringSubmatch(src, -1) {
		for _, item := range strings.Split(m[1], ",") {
			if n := methodName().FindStringSubmatch(strings.TrimSpace(item)); n != nil {
				ms = append(ms, n[1])
			}
		}
	}
	if len(ms) == 0 {
		return []string{"*"}
	}
	return dedupe(methodList(ms))
}

func dedupe(list []string) []string {
	var out []string
	for i, s := range list {
		if i == 0 || s != list[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// nextJS finds pages and route handlers of the app router (app/) and the
// pages router (pages/), also under src/.
func nextJS(root string) ([]routes.Route, error) {
	var out []routes.Route
	if app := firstDir(root, "app", "src/app"); app != "" {
		files, err := walkFiles(app)
		if err != nil {
			return nil, err
		}
		base := relSlash(root, app)
		for _, f := range files {
			segs := strings.Split(f, "/")
			dirs, file := segs[:len(segs)-1], segs[len(segs)-1]
			if hidden(dirs) || !scriptExt().MatchString(file) || testFile().MatchString(file) {
				continue
			}
			switch strings.TrimSuffix(file, filepath.Ext(file)) {
			case "page":
				out = append(out, routes.Route{Methods: []string{"GET"}, Path: urlPath(dirs, false), Action: "app/" + f, File: base + f})
			case "route":
				src, err := os.ReadFile(filepath.Join(app, filepath.FromSlash(f)))
				if err != nil {
					return nil, err
				}
				out = append(out, routes.Route{Methods: routeMethods(string(src)), Path: urlPath(dirs, false), Action: "app/" + f, File: base + f})
			case "sitemap":
				out = append(out, routes.Route{Methods: []string{"GET"}, Path: strings.TrimSuffix(urlPath(dirs, false), "/") + "/sitemap.xml", Action: "app/" + f, File: base + f})
			case "robots", "manifest":
				if len(dirs) == 0 {
					name := map[string]string{"robots": "/robots.txt", "manifest": "/manifest.webmanifest"}[strings.TrimSuffix(file, filepath.Ext(file))]
					out = append(out, routes.Route{Methods: []string{"GET"}, Path: name, Action: "app/" + f, File: base + f})
				}
			}
		}
	}
	if pages := firstDir(root, "pages", "src/pages"); pages != "" {
		files, err := walkFiles(pages)
		if err != nil {
			return nil, err
		}
		base := relSlash(root, pages)
		for _, f := range files {
			segs := strings.Split(strings.TrimSuffix(f, filepath.Ext(f)), "/")
			if !scriptExt().MatchString(f) || testFile().MatchString(f) || hidden(segs) {
				continue // _app, _document, _error and private files
			}
			if len(segs) == 1 && (segs[0] == "404" || segs[0] == "500") {
				continue // error pages, not routes
			}
			methods := []string{"GET"}
			if segs[0] == "api" {
				methods = []string{"*"} // an API route handles every method itself
			}
			out = append(out, routes.Route{Methods: methods, Path: urlPath(segs, true), Action: "pages/" + f, File: base + f})
		}
	}
	return out, nil
}

var (
	nuxtMethod = lazy.Regexp(`\.(get|head|post|put|patch|delete|options)$`)
	serverExt  = lazy.Regexp(`\.(ts|js|mjs)$`)
)

// nuxt finds pages (pages/ or app/pages/) and server routes (server/api/ under
// /api, server/routes/ at the root); a server file name may end in its method
// (users.post.ts).
func nuxt(root string) ([]routes.Route, error) {
	var out []routes.Route
	if pages := firstDir(root, "app/pages", "pages"); pages != "" {
		files, err := walkFiles(pages)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(root, pages)
		for _, f := range files {
			if !strings.HasSuffix(f, ".vue") {
				continue
			}
			segs := strings.Split(strings.TrimSuffix(f, ".vue"), "/")
			out = append(out, routes.Route{Methods: []string{"GET"}, Path: urlPath(segs, true), Action: filepath.ToSlash(rel) + "/" + f, File: filepath.ToSlash(rel) + "/" + f})
		}
	}
	for _, dir := range []struct{ name, prefix string }{{"server/api", "api"}, {"server/routes", ""}} {
		d := filepath.Join(root, dir.name)
		if !isDir(d) {
			continue
		}
		files, err := walkFiles(d)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !serverExt().MatchString(f) || testFile().MatchString(f) {
				continue
			}
			name := strings.TrimSuffix(f, filepath.Ext(f))
			methods := []string{"*"}
			if m := nuxtMethod().FindStringSubmatch(name); m != nil {
				methods = []string{strings.ToUpper(m[1])}
				name = strings.TrimSuffix(name, m[0])
			}
			segs := strings.Split(name, "/")
			if dir.prefix != "" {
				segs = append([]string{dir.prefix}, segs...)
			}
			out = append(out, routes.Route{Methods: methods, Path: urlPath(segs, true), Action: dir.name + "/" + f, File: dir.name + "/" + f})
		}
	}
	return out, nil
}
