package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/lazy"
)

// skipDirs are folders that never hold a service of their own.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true, "target": true, "tmp": true,
	"storage": true, "public": true, "docs": true, "doc": true, "tests": true, "test": true, "bin": true,
	"config": true, "resources": true, "database": true, "driftgate": true, "coverage": true, "out": true,
}

// candidateDirs lists the folders init looks at: the root, its subfolders,
// and the folders under apps/, services/ and packages/ (monorepos).
func candidateDirs(root string) []string {
	dirs := []string{"."}
	list := func(rel string) []string {
		entries, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, ".") || skipDirs[n] {
				continue
			}
			out = append(out, filepath.ToSlash(filepath.Join(rel, n)))
		}
		return out
	}
	for _, d := range list(".") {
		switch filepath.Base(d) {
		case "apps", "services", "packages":
			dirs = append(dirs, list(d)...)
		default:
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// service is one recognised service of a project.
type service struct {
	name, dir string
	d         detected
}

var nameChars = lazy.Regexp(`[^a-z0-9]+`)

// serviceName is the folder's name (the project's for the root) as a
// simple lower-case name, the same form request IDs take.
func serviceName(dir, project string) string {
	n := filepath.Base(dir)
	if dir == "." {
		n = project
	}
	n = strings.Trim(nameChars().ReplaceAllString(strings.ToLower(n), "-"), "-")
	if n == "" {
		return "service"
	}
	return n
}

// findServices recognises a service in every candidate folder.
func findServices(root, project string) []service {
	var out []service
	used := map[string]bool{}
	for _, dir := range candidateDirs(root) {
		d := detectDir(filepath.Join(root, dir))
		if len(d.kinds) == 0 {
			continue
		}
		name := serviceName(dir, project)
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", serviceName(dir, project), n)
		}
		used[name] = true
		out = append(out, service{name: name, dir: dir, d: d})
	}
	return out
}

// composePorts reads docker-compose: the host port each service publishes,
// by build folder and by service name.
func composePorts(root string) map[string]int {
	out := map[string]int{}
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]struct {
				Build any   `yaml:"build"`
				Ports []any `yaml:"ports"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(raw, &doc) != nil {
			continue
		}
		names := make([]string, 0, len(doc.Services))
		for n := range doc.Services {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := doc.Services[n]
			port := 0
			for _, p := range s.Ports {
				if port = publishedPort(p); port > 0 {
					break
				}
			}
			if port == 0 {
				continue
			}
			out["service:"+n] = port
			context := ""
			switch b := s.Build.(type) {
			case string:
				context = b
			case map[string]any:
				context, _ = b["context"].(string)
				if context == "" {
					context = "." // build: {dockerfile: …} builds the root
				}
			}
			if context != "" {
				out["build:"+filepath.ToSlash(filepath.Clean(context))] = port
			}
		}
		break
	}
	return out
}

// publishedPort reads "8000:80", "127.0.0.1:8000:80", "8000:80/tcp" or
// {published: 8000}.
func publishedPort(p any) int {
	switch v := p.(type) {
	case string:
		parts := strings.Split(strings.Split(v, "/")[0], ":")
		if len(parts) >= 2 {
			// "8005-8006:80-81": the first port of the range
			n, _ := strconv.Atoi(strings.SplitN(parts[len(parts)-2], "-", 2)[0])
			return n
		}
	case map[string]any:
		switch pub := v["published"].(type) {
		case int:
			return pub
		case string:
			n, _ := strconv.Atoi(pub)
			return n
		}
	}
	return 0
}

// tag adds the service to every entry of a block written for one service.
func tag(block, field, value string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(block, "\n") {
		if strings.HasPrefix(line, "  - ") {
			b.WriteString("  - " + field + ": " + value + "\n    " + strings.TrimPrefix(line, "  - "))
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// detect recognises the services of a project. One service in the project
// root keeps the simple form (target:); several get targets: with each
// adapter, exclusion, allowlist entry and observe entry tied to its service.
// With only, it keeps the services named there.
func detect(root, project string, only []string) (detected, error) {
	if project == "" {
		project = filepath.Base(root)
	}
	services := findServices(root, project)
	var found []string
	for _, s := range services {
		found = append(found, s.name)
	}
	if len(only) > 0 {
		var kept []service
		for _, name := range only {
			i := slices.IndexFunc(services, func(s service) bool { return s.name == name })
			if i < 0 {
				return detected{}, configErr("--service %s: not found (found: %s)", name, strings.Join(found, ", "))
			}
			kept = append(kept, services[i])
		}
		services = kept
	}
	if len(services) == 0 {
		return detected{}, nil
	}
	if len(services) == 1 && services[0].dir == "." {
		return services[0].d, nil
	}
	ports := composePorts(root)
	var d detected
	var targets, disc, observe strings.Builder
	usedBase := map[string]string{}
	targets.WriteString("targets:\n")
	disc.WriteString("discover:\n")
	for _, s := range services {
		d.kinds = append(d.kinds, s.name+" ("+strings.Join(s.d.kinds, ", ")+", "+s.dir+")")
		for _, k := range s.d.unchecked {
			d.unchecked = append(d.unchecked, s.name+": "+k)
		}
		base := s.d.baseURL
		source := "the framework's default port"
		if port, ok := ports["build:"+s.dir]; ok {
			base, source = fmt.Sprintf("http://localhost:%d", port), "docker-compose"
		} else if port, ok := ports["service:"+s.name]; ok {
			base, source = fmt.Sprintf("http://localhost:%d", port), "docker-compose"
		}
		if other, taken := usedBase[base]; taken {
			source += "; " + other + " has the same address: set this service's port"
		}
		usedBase[base] = s.name
		d.services = append(d.services, s.name)
		if !strings.HasPrefix(source, "docker-compose") {
			d.defaults = append(d.defaults, s.name)
		}
		fmt.Fprintf(&targets, "  %s:\n    base_url: %s   # from %s\n", s.name, base, source)
		// Every adapter of the service gets its target and folder.
		entries := s.d.block[len("discover:\n"):strings.Index(s.d.block, "\ntemplates:")]
		replacement := "  - adapter: $1\n    target: " + s.name
		if s.dir != "." {
			replacement += "\n    dir: " + s.dir
		}
		disc.WriteString(regexp.MustCompile(`(?m)^  - adapter: (\S+)`).ReplaceAllString(entries, replacement))
		for _, x := range s.d.excluded {
			d.excludedFor = append(d.excludedFor, [3]string{s.name, x[0], x[1]})
		}
		d.allow += tag(strings.ReplaceAll(s.d.allow, "  - request: '*'\n    ", "  - "), "target", s.name)
		d.headers += tag(s.d.headers, "target", s.name)
		observe.WriteString(tag(s.d.observe, "target", s.name))
	}
	d.targets = targets.String()
	d.found = found
	d.block = disc.String() + "\ntemplates: [ok, not-found, json]\n"
	if observe.Len() > 0 {
		d.block += "\nobserve:\n" + observe.String()
	}
	return d, nil
}
