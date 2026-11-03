// Package appconf reads the database and the address an application is set
// up with, from the files each technology keeps them in: .env, appsettings
// .json, application.properties, database.yml, env.php, settings.py,
// compose.yaml and the like. It never runs the project's code: PHP, Python
// and ERB are read as text, and what only code could compute is reported as
// such.
//
// A password is read only to build a DSN when a command runs (Ref.DSN).
// Nothing Find returns holds one, and no error of this package quotes a
// value read from a file or the environment: errors name the file and the
// key.
package appconf

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Lookup returns an environment variable, as os.LookupEnv does.
type Lookup func(string) (string, bool)

// Sources of a Ref: the format of the file it names, or the environment.
const (
	Env         = "env"         // Key is the variable
	Dotenv      = "dotenv"      // .env files
	AppSettings = "appsettings" // ASP.NET appsettings*.json, or a .csproj for its layered settings
	Properties  = "properties"  // Spring, Quarkus and Micronaut application*.properties / .yml
	YAML        = "yaml"        // Rails config/database.yml, a Go config.yaml
	PHP         = "php"         // Magento app/etc/env.php
	Python      = "python"      // Django settings.py
	Compose     = "compose"     // a database service of compose.yaml
)

// Sources lists every source of a file, in the order the documentation
// gives them.
var Sources = []string{Dotenv, AppSettings, Properties, YAML, PHP, Python, Compose}

// Ref names a database setting in the project instead of holding it: what
// driftgate.yaml stores in place of a DSN. It is read when a command runs.
type Ref struct {
	Source string // one of Sources, or Env
	File   string // relative to the project root, with / (empty for Env)
	Key    string // where the setting is in the file (per source); for Env the variable
	// Host replaces the host and port of the setting: the address the
	// database is reached at from this machine, when the application's
	// own is the name of a compose service (127.0.0.1:3307).
	Host string
}

// IsZero reports a Ref that names nothing.
func (r Ref) IsZero() bool { return r == Ref{} }

// Database is a database setting Find found. It never holds the password.
type Database struct {
	Driver string // mysql (also MariaDB) or postgres; empty when unknown (Note says why)
	Host   string // as this machine reaches it (Ref.Host applied)
	Port   string
	Name   string // the database the application uses: the working database
	User   string
	File   string // where it was found: a file relative to the root with /, or the variable
	Key    string
	Ref    Ref    // what driftgate.yaml would hold; zero when it cannot be referenced (Note says why)
	Note   string // what to check by hand, if anything
	// HostNeeded is the host: the setting needs before DriftGate can use it
	// (127.0.0.1:3306), "" for none: its host is one its client may take
	// for the local socket (MySQL's localhost or none, PostgreSQL's none),
	// and DriftGate does not guess. Note says so.
	HostNeeded string
}

// AppURL is an address the application is set up to answer at.
type AppURL struct {
	URL  string
	File string
	Key  string
}

// Note is a setting found that DriftGate cannot use as it is: another kind
// of database, a value set by code, a file it cannot read.
type Note struct {
	File string
	Text string
}

// Found is everything Find found, in the order of its sources.
type Found struct {
	Databases []Database
	AppURLs   []AppURL
	Notes     []Note
}

// CheckFile checks the file of a Ref: relative to the project root and
// inside it. A \ is taken for a /, so one configuration serves Windows too.
func CheckFile(file string) error {
	f := strings.ReplaceAll(file, `\`, "/")
	switch {
	case f == "":
		return errors.New("needs a file")
	case strings.ContainsRune(f, 0):
		return errors.New("the file name holds a NUL")
	case strings.HasPrefix(f, "/") || filepath.VolumeName(f) != "" || (len(f) > 1 && f[1] == ':'):
		return fmt.Errorf("%s must be relative to the project root", file)
	}
	if c := path.Clean(f); c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("%s must be inside the project", file)
	}
	return nil
}

// CheckRef checks the shape of a Ref, without reading anything: a known
// source, a file inside the project, the key a source needs, a host that is
// host or host:port.
func CheckRef(r Ref) error {
	switch r.Source {
	case Env:
		if r.Key == "" || r.File != "" || r.Host != "" {
			return errors.New("env takes the name of a variable only")
		}
		return nil
	case Dotenv, Properties, YAML, PHP, Python:
	case AppSettings, Compose:
		if r.Key == "" {
			what := "the connection string, e.g. ConnectionStrings:Default"
			if r.Source == Compose {
				what = "the name of the database service"
			}
			return fmt.Errorf("%s needs key (%s)", r.Source, what)
		}
	default:
		return fmt.Errorf("unknown source %q", r.Source)
	}
	if err := CheckFile(r.File); err != nil {
		return err
	}
	if svc, ok := strings.CutPrefix(r.Host, ComposeHost); ok {
		if printableName(svc) != svc {
			return errors.New("host compose: needs the name of a compose service")
		}
	} else if r.Host != "" {
		var p parts
		if err := p.setHost(r.Host); err != nil {
			return fmt.Errorf("host %w", err)
		}
	}
	return nil
}

// ComposeHost starts a Ref.Host that names a service of the project's
// compose file (compose:mysql): the setting's host is that service, and
// this machine reaches it at the port the service publishes, read when a
// command runs.
const ComposeHost = "compose:"

// localPath is the path of a file of the project: the file itself, past
// any symbolic links, which must stay inside the project's own folder
// (also past links), so a link cannot lead a setting to a file elsewhere.
func localPath(root, file string) (string, error) {
	if err := CheckFile(file); err != nil {
		return "", err
	}
	realRoot, err := filepath.Abs(root)
	if err == nil {
		realRoot, err = filepath.EvalSymlinks(realRoot)
	}
	if err != nil {
		return "", fmt.Errorf("the project folder: %w", unwrapPath(err))
	}
	p := filepath.Join(realRoot, filepath.FromSlash(path.Clean(strings.ReplaceAll(file, `\`, "/"))))
	real, err := filepath.EvalSymlinks(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s: not found", file)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", file, unwrapPath(err))
	}
	if rel, err := filepath.Rel(realRoot, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: a symbolic link leads outside the project", file)
	}
	return real, nil
}

// LocalPath is the real path of a file of the project, relative to root:
// past any symbolic links, which must stay inside the project's own folder
// (also past links). The file must exist. An error names the file, never
// what it holds.
func LocalPath(root, file string) (string, error) { return localPath(root, file) }

// unwrapPath drops the path from an error of os, which names the file in
// its own words.
func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// readFile reads a file of the project (see localPath), naming it (not its
// content) in an error.
func readFile(root, file string) ([]byte, error) {
	p, err := localPath(root, file)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, unwrapPath(err))
	}
	return raw, nil
}

// DSN reads the setting r names and returns it as a DSN the database
// drivers of DriftGate accept: mysql://user:pass@host:port/ (the driver's
// own form for a socket or when the file holds it) or postgres://user:pass
// @host:port/. The database of the setting is left out (state: and data:
// name the databases), as are the parameters of the application's driver
// that DriftGate's do not take. driver is the driver of the component; a
// file that names another one is an error.
func (r Ref) DSN(root, driver string, env Lookup) (string, error) {
	dsn, _, err := r.Read(root, driver, env)
	return dsn, err
}

// Read is DSN that also returns the database the setting names (the one
// the application runs on), "" when it names none.
func (r Ref) Read(root, driver string, env Lookup) (dsn, database string, err error) {
	p, err := r.read(root, env)
	if err != nil {
		return "", "", err
	}
	if hp := p.hostNeeded(r, driver); hp != "" {
		return "", "", fmt.Errorf("%s: %s", r.where(), hostNeededText(p, hp))
	}
	dsn, err = p.dsn(driver, r.where())
	return dsn, p.name, err
}

// socketClients are the sources whose clients (PDO, mysql2, mysqlclient,
// libpq) take MySQL's localhost or no host, and PostgreSQL's no host, for
// the local socket; JDBC, .NET and compose services are TCP.
var socketClients = []string{Dotenv, YAML, PHP, Python}

// hostNeeded is the host: a setting needs before DriftGate can use it, "" for
// none: its host is one that its client may take for the local socket, and
// DriftGate does not guess which.
func (p parts) hostNeeded(r Ref, driver string) string {
	if driver == "" {
		driver = p.driver
	}
	if r.Host != "" || p.socket != "" || p.form != "" || !slices.Contains(socketClients, r.Source) {
		return ""
	}
	port := p.port
	switch {
	case driver == MySQL && (p.host == "" || strings.EqualFold(p.host, "localhost")):
		if port == "" {
			port = "3306"
		}
	case driver == Postgres && p.host == "":
		if port == "" {
			port = "5432"
		}
	default:
		return ""
	}
	return "127.0.0.1:" + port
}

// hostNeededText says what to add for hostNeeded.
func hostNeededText(p parts, hostport string) string {
	h := p.host
	if h == "" {
		h = "not set"
	}
	return "the host is " + h + ", which the application's client may take for the local socket: add host: " + hostport + " to the dsn when the database listens on TCP there, or host: <path of the socket>"
}

// where names a Ref in an error.
func (r Ref) where() string {
	switch {
	case r.Source == Env:
		return "environment variable " + r.Key
	case r.Key == "":
		return r.File
	}
	return r.File + ": " + r.Key
}

// read reads the setting r names.
func (r Ref) read(root string, env Lookup) (parts, error) {
	if env == nil {
		env = func(string) (string, bool) { return "", false }
	}
	var p parts
	var err error
	if r.Source != Env {
		if err := CheckFile(r.File); err != nil {
			return parts{}, err
		}
	}
	switch r.Source {
	case Env:
		v, ok := env(r.Key)
		if !ok || v == "" {
			return parts{}, fmt.Errorf("environment variable %s is not set", r.Key)
		}
		p, err = fromValue(v)
	case Dotenv:
		p, err = readDotenv(root, r.File, r.Key, env)
	case AppSettings:
		p, err = readAppSettings(root, r.File, r.Key, env)
	case Properties:
		p, err = readProperties(root, r.File, r.Key, env)
	case YAML:
		p, err = readYAML(root, r.File, r.Key, env)
	case PHP:
		p, err = readPHP(root, r.File, r.Key)
	case Python:
		p, err = readPython(root, r.File, r.Key)
	case Compose:
		p, err = readCompose(root, r.File, r.Key, env)
	default:
		return parts{}, fmt.Errorf("unknown source %q", r.Source)
	}
	if err != nil {
		if r.Source == Env || strings.HasPrefix(err.Error(), r.File+":") {
			return parts{}, err
		}
		return parts{}, fmt.Errorf("%s: %w", r.where(), err)
	}
	host := r.Host
	if svc, ok := strings.CutPrefix(host, ComposeHost); ok {
		if host, err = composeAddress(root, svc, p, env); err != nil {
			return parts{}, fmt.Errorf("%s: host: %w", r.where(), err)
		}
	}
	if host != "" {
		if err := p.setHost(host); err != nil {
			return parts{}, fmt.Errorf("%s: host: %w", r.where(), err)
		}
	}
	return p, nil
}

// composeAddress is the address this machine reaches the database service
// svc of the project's compose file at, for the port of setting p.
func composeAddress(root, svc string, p parts, env Lookup) (string, error) {
	file := ""
	for _, n := range composeFiles {
		if _, err := localPath(root, n); err == nil {
			file = n
			break
		}
	}
	if file == "" {
		return "", errors.New("the project has no compose file")
	}
	cp, err := readComposeProject(root, file, env)
	if err != nil {
		return "", err
	}
	s, ok := cp.services[svc]
	if !ok {
		return "", fmt.Errorf("%s has no service %s", file, svc)
	}
	target, driver := p.port, p.driver
	if driver == "" {
		image, _ := interpolate(s.image, cp.vars)
		driver = imageDriver(image)
	}
	if target == "" {
		target = "3306"
		if driver == Postgres {
			target = "5432"
		}
	}
	addr, published, err := cp.published(svc, target)
	if err != nil {
		return "", err
	}
	if !published {
		return "", fmt.Errorf("service %s of %s publishes no port %s on this machine", svc, file, target)
	}
	return addr, nil
}
