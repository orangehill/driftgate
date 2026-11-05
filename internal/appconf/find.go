package appconf

import (
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Find looks for the database settings and the address of the application
// in root, where each technology keeps them: PHP (.env, Magento's
// app/etc/env.php), .NET (appsettings*.json through each .csproj, user
// secrets, launchSettings.json), Java (application*.properties and .yml),
// Rails (config/database.yml), Django (settings.py), Node (.env*,
// prisma/schema.prisma), Go (.env, config.yaml), compose (the database
// service and the port it publishes) and the environment (env, nil for
// none: DATABASE_URL, ConnectionStrings__*, SPRING_DATASOURCE_*). It reads
// files only; nothing it returns holds a password.
func Find(root string, env Lookup) Found {
	if env == nil {
		env = func(string) (string, bool) { return "", false }
	}
	f := &finder{root: root, env: env}
	f.dotenv(f.prisma())
	f.magento()
	f.dotnet()
	f.java()
	f.rails()
	f.django()
	f.goConfig()
	f.compose()
	f.environment()
	return f.out
}

type finder struct {
	root string
	env  Lookup
	out  Found
}

func (f *finder) note(file, text string) {
	f.out.Notes = append(f.out.Notes, Note{File: file, Text: strings.TrimPrefix(text, file+": ")})
}

// add reads r and records it: a database, or a note of why it cannot be
// used. hint is the driver the project's packages say, other another kind
// of database they say.
func (f *finder) add(r Ref, hint, other string) {
	file := r.File
	if r.Source == Env {
		file = r.Key
	}
	p, err := r.read(f.root, f.env)
	if err != nil {
		f.note(file, err.Error())
		return
	}
	if p.driver == "" && p.other == "" {
		p.driver, p.other = hint, other
	}
	if p.other != "" {
		f.note(file, r.where()+" names "+p.other+"; "+restores())
		return
	}
	d := p.database()
	d.File, d.Key, d.Ref = file, r.Key, r
	if r.Source == Env {
		d.Note = "set in this shell's environment; it must be set when DriftGate runs"
	}
	if d.Driver == "" {
		d.Note = join(d.Note, "the setting does not say which database it is: set driver ("+driverList()+")")
	}
	if p.driver == SQLite {
		if n := f.sqliteNote(p); n != "" {
			d.Note = join(d.Note, n)
		}
	}
	if d.Driver == SQLServer {
		d.Note = join(d.Note, p.sqlServerNote())
	}
	if hp := p.hostNeeded(r, d.Driver); hp != "" {
		d.HostNeeded = hp
		d.Note = join(d.Note, hostNeededText(p, hp))
	}
	f.out.Databases = append(f.out.Databases, d)
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func (f *finder) exists(file string) bool {
	_, err := localPath(f.root, file)
	return err == nil
}

// glob lists the files of patterns under root, relative with /, sorted.
func (f *finder) glob(patterns ...string) []string {
	var out []string
	for _, p := range patterns {
		m, _ := filepath.Glob(filepath.Join(f.root, filepath.FromSlash(p)))
		for _, abs := range m {
			if rel, err := filepath.Rel(f.root, abs); err == nil && !slices.Contains(out, filepath.ToSlash(rel)) {
				out = append(out, filepath.ToSlash(rel))
			}
		}
	}
	slices.Sort(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// prismaURL is the variable schema.prisma takes the URL from, and the
// driver of its provider.
type prismaURL struct{ name, driver string }

// prisma reads datasource url = env("NAME") of prisma/schema.prisma.
func (f *finder) prisma() prismaURL {
	raw, err := readFile(f.root, "prisma/schema.prisma")
	if err != nil {
		return prismaURL{}
	}
	s := string(raw)
	i := strings.Index(s, "datasource")
	if i < 0 {
		return prismaURL{}
	}
	block := s[i:]
	if end := strings.IndexByte(block, '}'); end >= 0 {
		block = block[:end]
	}
	var out prismaURL
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "provider":
			out.driver, _ = driverName(strings.Trim(v, `"`))
		case "url":
			if name, ok := strings.CutPrefix(v, `env("`); ok {
				out.name, _, _ = strings.Cut(name, `"`)
			} else if strings.HasPrefix(v, `"`) {
				f.note("prisma/schema.prisma", "datasource url is written in the file; move it to .env (url = env(\"DATABASE_URL\")) so it is not copied")
			}
		}
	}
	return out
}

// urlKeysDotenv are the keys of a .env that hold a database URL.
var urlKeysDotenv = []string{"DB_URL", "DATABASE_URL"}

// appURLKeys are the keys of a .env that hold the application's address.
var appURLKeys = []string{"APP_URL", "NEXTAUTH_URL", "NEXT_PUBLIC_APP_URL"}

// dotenv reads the .env files a framework reads, most specific first:
// Laravel's DB_* keys, the URL keys, and the address.
func (f *finder) dotenv(pr prismaURL) {
	keys := urlKeysDotenv
	if pr.name != "" && !slices.Contains(keys, pr.name) {
		keys = append(slices.Clip(keys), pr.name)
	}
	for _, file := range f.dotenvFiles() {
		raw, err := readFile(f.root, file)
		if err != nil {
			continue
		}
		vals, err := parseDotenv(raw, f.env)
		if err != nil {
			f.note(file, err.Error())
			continue
		}
		if laravel(vals) {
			f.add(Ref{Source: Dotenv, File: file}, "", "")
		}
		for _, k := range keys {
			if strings.TrimSpace(vals[k]) != "" {
				hint := ""
				if k == pr.name {
					hint = pr.driver
				}
				f.add(Ref{Source: Dotenv, File: file, Key: k}, hint, "")
			}
		}
		for _, k := range appURLKeys {
			if v := strings.TrimSpace(vals[k]); v != "" {
				f.out.AppURLs = append(f.out.AppURLs, AppURL{URL: v, File: file, Key: k})
			}
		}
	}
}

// dotenvFiles are the .env files the framework reads, the one that wins
// first. Laravel (artisan): .env, or .env.<APP_ENV> instead when APP_ENV
// is set in the environment and the file exists. Symfony (bin/console):
// .env, .env.local (not for test), .env.<APP_ENV>, .env.<APP_ENV>.local,
// the later winning, APP_ENV from the environment, else the files, else
// dev. Others (Next.js, Vite, Node): .env.development.local, .env.local,
// .env.development, .env.
func (f *finder) dotenvFiles() []string {
	appEnv, set := f.env("APP_ENV")
	if printableName(appEnv) != appEnv { // names a file
		appEnv, set = "", false
	}
	switch {
	case f.exists("artisan"):
		if set && appEnv != "" && f.exists(".env."+appEnv) {
			return []string{".env." + appEnv}
		}
		return []string{".env"}
	case f.exists("bin/console") || f.exists("symfony.lock"):
		if !set || appEnv == "" {
			appEnv = "dev"
			for _, file := range []string{".env", ".env.local"} {
				if raw, err := readFile(f.root, file); err == nil {
					if vals, err := parseDotenv(raw, f.env); err == nil && vals["APP_ENV"] != "" && printableName(vals["APP_ENV"]) == vals["APP_ENV"] {
						appEnv = vals["APP_ENV"]
					}
				}
			}
		}
		files := []string{".env." + appEnv + ".local", ".env." + appEnv}
		if appEnv != "test" {
			files = append(files, ".env.local")
		}
		return append(files, ".env")
	}
	return []string{".env.development.local", ".env.local", ".env.development", ".env"}
}

func (f *finder) magento() {
	if f.exists("app/etc/env.php") {
		f.add(Ref{Source: PHP, File: "app/etc/env.php", Key: "db.connection.default"}, MySQL, "")
	}
}

// dotnet reads each project file's layered settings (appsettings.json,
// appsettings.Development.json, user secrets) and launchSettings.json.
func (f *finder) dotnet() {
	for _, proj := range f.glob("*.csproj", "*/*.csproj", "src/*/*.csproj") {
		raw, err := readFile(f.root, proj)
		if err != nil {
			continue
		}
		hint, other := csprojDriver(string(raw))
		dir := path.Dir(proj)
		if ls, err := readFile(f.root, path.Join(dir, "Properties/launchSettings.json")); err == nil {
			urls, _ := launchProfile(ls)
			for _, u := range urls {
				f.out.AppURLs = append(f.out.AppURLs, AppURL{URL: u, File: path.Join(dir, "Properties/launchSettings.json"), Key: "applicationUrl"})
			}
		}
		m, err := appSettings(f.root, proj, f.env)
		if err != nil {
			if !strings.HasSuffix(err.Error(), "no appsettings.json next to it") {
				f.note(proj, err.Error())
			}
			continue
		}
		for _, k := range sortedKeys(m) {
			name, ok := strings.CutPrefix(k, "connectionstrings:")
			if !ok || strings.Contains(name, ":") {
				continue
			}
			key := m[k][0]
			f.add(Ref{Source: AppSettings, File: proj, Key: key}, hint, other)
			envName := "ConnectionStrings__" + key[len("ConnectionStrings:"):]
			if v, ok := f.env(envName); ok && v != "" {
				f.add(Ref{Source: Env, Key: envName}, hint, other)
			}
		}
	}
}

// java reads the datasource of each Spring, Quarkus or Micronaut file, and
// the port the server listens on.
func (f *finder) java() {
	// application.* and the files of the active profiles: another profile's
	// file is not what the application runs with.
	var files []string
	for _, dir := range f.glob("src/main/resources", "*/src/main/resources") {
		dir += "/"
		active, err := springActive(f.root, dir, f.env)
		if err != nil {
			f.note(dir, err.Error())
			continue
		}
		stems := []string{"application"}
		for _, p := range active {
			if printableName(p) == p {
				stems = append(stems, "application-"+p)
			}
		}
		for _, stem := range stems {
			for _, ext := range []string{".properties", ".yml", ".yaml"} {
				if f.exists(dir + stem + ext) {
					files = append(files, dir+stem+ext)
				}
			}
		}
	}
	for _, file := range files {
		m, err := readPropertyFile(f.root, file, nil)
		if err != nil {
			f.note(file, err.Error())
			continue
		}
		for _, k := range []string{"server.port", "quarkus.http.port", "micronaut.server.port"} {
			if p, err := placeholders(m[k], m, f.env); err == nil && p != "" && strings.Trim(p, "0123456789") == "" {
				f.out.AppURLs = append(f.out.AppURLs, AppURL{URL: "http://localhost:" + p, File: file, Key: k})
			}
		}
		set := false
		for _, pre := range propertyPrefixes {
			set = set || datasourceSet(m, pre) || datasourceSet(m, "%dev."+pre)
		}
		if set {
			f.add(Ref{Source: Properties, File: file}, "", "")
		}
	}
	if u, ok := f.env("SPRING_DATASOURCE_URL"); ok && u != "" {
		p, err := fromValue(u)
		if err != nil {
			f.note("SPRING_DATASOURCE_URL", err.Error())
			return
		}
		d := p.database()
		if v, ok := f.env("SPRING_DATASOURCE_USERNAME"); ok {
			d.User = v
		}
		d.File = "SPRING_DATASOURCE_URL"
		d.Note = "Spring takes SPRING_DATASOURCE_URL, _USERNAME and _PASSWORD from the environment over its files; DriftGate takes one DSN: put it in one variable and use dsn: { env: NAME }"
		f.out.Databases = append(f.out.Databases, d)
	}
}

// rails reads the development database of config/database.yml (its
// primary one when there are several).
func (f *finder) rails() {
	const file = "config/database.yml"
	if !f.exists(file) {
		return
	}
	r := Ref{Source: YAML, File: file, Key: "development"}
	if p, err := r.read(f.root, f.env); err == nil && p.driver == "" && p.other == "" && p.name == "" {
		r.Key = "development.primary"
	}
	f.add(r, "", "")
	if r.Key == "development.primary" {
		f.railsSQLite(file, "development")
	}
}

// django reads DATABASES of the settings files that set it.
func (f *finder) django() {
	for _, file := range f.glob("settings.py", "*/settings.py", "*/settings/*.py", "config/settings/*.py") {
		raw, err := readFile(f.root, file)
		if err != nil || !strings.Contains(string(raw), "DATABASES") {
			continue
		}
		if _, err := djangoDatabases(string(raw)); err != nil {
			continue // DATABASES only used here, or set another way
		}
		f.add(Ref{Source: Python, File: file, Key: "default"}, "", "")
	}
}

// goConfig reads the database of a Go service's config.yaml.
func (f *finder) goConfig() {
	if !f.exists("go.mod") {
		return
	}
	for _, file := range []string{"config.yaml", "config.yml", "config/config.yaml", "config/config.yml", "configs/config.yaml", "configs/config.yml"} {
		if !f.exists(file) {
			continue
		}
		r := Ref{Source: YAML, File: file}
		if _, err := r.read(f.root, f.env); err != nil && strings.Contains(err.Error(), "(name the key)") {
			continue
		}
		f.add(r, "", "")
	}
}

// webPorts are container ports an application usually listens on.
var webPorts = []string{"80", "8080", "8000", "3000", "5000", "4000", "8081", "8888", "5173", "443", "8443"}

// compose reads the database services of the compose file, the address of
// the application's container, and gives the settings that name a database
// service as their host the address it is published at on this machine.
func (f *finder) compose() {
	var file string
	for _, n := range composeFiles {
		if f.exists(n) {
			file = n
			break
		}
	}
	if file == "" {
		return
	}
	cp, err := readComposeProject(f.root, file, f.env)
	if err != nil {
		f.note(file, err.Error())
		return
	}
	dbs := map[string]string{} // a name the application may use as host: the service
	for _, name := range sortedKeys(cp.services) {
		s := cp.services[name]
		image, _ := interpolate(s.image, cp.vars)
		if imageDriver(image) != "" {
			f.add(Ref{Source: Compose, File: file, Key: name}, "", "")
			dbs[name] = name
			if s.containerName != "" {
				dbs[s.containerName] = name
			}
			continue
		}
		for _, wp := range webPorts {
			if addr, ok, _ := cp.published(name, wp); ok {
				scheme := "http"
				if wp == "443" || wp == "8443" {
					scheme = "https"
				}
				f.out.AppURLs = append(f.out.AppURLs, AppURL{URL: scheme + "://" + strings.Replace(addr, "127.0.0.1", "localhost", 1), File: file, Key: "services." + name + ".ports"})
				break
			}
		}
	}
	for i, d := range f.out.Databases {
		svc, ok := dbs[d.Host]
		if !ok || d.Ref.Source == Compose || d.Ref.Source == Env {
			continue
		}
		target := defaultPort(imageDriver(cp.services[svc].image))
		if d.Port != "" {
			target = d.Port
		}
		addr, published, _ := cp.published(svc, target)
		if !published {
			d.Note = join(d.Note, "the host is the compose service "+svc+", which publishes no port "+target+" on this machine")
		} else {
			d.Ref.Host = ComposeHost + svc // the published port is read when a command runs
			d.Note = join(d.Note, "the host is the compose service "+svc+"; from this machine it is "+addr)
			p := parts{host: d.Host}
			if p.setHost(addr) == nil {
				d.Host, d.Port = p.host, p.port
			}
		}
		f.out.Databases[i] = d
	}
}

// environment reads the URL variables of this shell.
func (f *finder) environment() {
	for _, k := range urlKeysDotenv {
		if v, ok := f.env(k); ok && v != "" {
			f.add(Ref{Source: Env, Key: k}, "", "")
		}
	}
}
