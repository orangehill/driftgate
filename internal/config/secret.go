package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/secret"
)

// Secret is a DSN that never sits in the configuration itself: it comes
// from an environment variable, a 1Password reference, or a setting of the
// project's own files (internal/appconf), read when a command runs.
type Secret struct {
	Env string `yaml:"env,omitempty"`
	Op  string `yaml:"op,omitempty"`
	// A file of the project, relative to its root, by its format; Key says
	// where the setting is in it and Host replaces its host[:port].
	Dotenv      string `yaml:"dotenv,omitempty"`
	AppSettings string `yaml:"appsettings,omitempty"`
	Properties  string `yaml:"properties,omitempty"`
	YAML        string `yaml:"yaml,omitempty"`
	PHP         string `yaml:"php,omitempty"`
	Python      string `yaml:"python,omitempty"`
	Compose     string `yaml:"compose,omitempty"`
	Key         string `yaml:"key,omitempty"`
	Host        string `yaml:"host,omitempty"`
}

// IsZero lets omitempty skip an unset secret.
func (s Secret) IsZero() bool { return s == Secret{} }

// files are the file sources of s, by appconf source.
func (s Secret) files() [7][2]string {
	return [7][2]string{
		{appconf.Dotenv, s.Dotenv}, {appconf.AppSettings, s.AppSettings}, {appconf.Properties, s.Properties},
		{appconf.YAML, s.YAML}, {appconf.PHP, s.PHP}, {appconf.Python, s.Python}, {appconf.Compose, s.Compose},
	}
}

// ref is the setting of a file s names; zero for env and op.
func (s Secret) ref() appconf.Ref {
	for _, f := range s.files() {
		if f[1] != "" {
			return appconf.Ref{Source: f[0], File: f[1], Key: s.Key, Host: s.Host}
		}
	}
	return appconf.Ref{}
}

// SecretFor is the dsn: of driftgate.yaml for a setting appconf.Find found.
func SecretFor(r appconf.Ref) Secret {
	s := Secret{Key: r.Key, Host: r.Host}
	switch r.Source {
	case appconf.Env:
		return Secret{Env: r.Key}
	case appconf.Dotenv:
		s.Dotenv = r.File
	case appconf.AppSettings:
		s.AppSettings = r.File
	case appconf.Properties:
		s.Properties = r.File
	case appconf.YAML:
		s.YAML = r.File
	case appconf.PHP:
		s.PHP = r.File
	case appconf.Python:
		s.Python = r.File
	case appconf.Compose:
		s.Compose = r.File
	}
	return s
}

// dsnSources names the sources of a DSN in messages.
const dsnSources = "env, op, " + appconf.Dotenv + ", " + appconf.AppSettings + ", " + appconf.Properties + ", " +
	appconf.YAML + ", " + appconf.PHP + ", " + appconf.Python + " or " + appconf.Compose

// validate checks the shape of a set secret: one source, and key and host
// only with a file. Nothing is read.
func (s Secret) validate() error {
	var set []string
	if s.Env != "" {
		set = append(set, "env")
	}
	if s.Op != "" {
		set = append(set, "op")
	}
	for _, f := range s.files() {
		if f[1] != "" {
			set = append(set, f[0])
		}
	}
	switch {
	case len(set) == 0:
		return errors.New("needs one of " + dsnSources)
	case len(set) > 1:
		return fmt.Errorf("takes one source, not %s", strings.Join(set, " and "))
	case s.Env != "" || s.Op != "":
		if s.Key != "" || s.Host != "" {
			return errors.New("key and host are for a file of the project (" + dsnSources[len("env, op, "):] + ")")
		}
		return nil
	}
	return appconf.CheckRef(s.ref())
}

// Read returns the DSN for a database with driver (mysql, postgres, sqlite
// or sqlserver) of a
// project rooted at root. A DSN of the environment or 1Password may also be
// a JDBC URL, an ADO.NET connection string or libpq's keyword form; one of
// a file is converted from the file's form (appconf.Ref.DSN). No error
// quotes a value.
func (s Secret) Read(ctx context.Context, root, driver string) (string, error) {
	dsn, _, err := s.read(ctx, root, driver)
	return dsn, err
}

// read is Read that also returns the database a file's setting names ("" for
// none, and for env and op, whose database is not the application's).
func (s Secret) read(ctx context.Context, root, driver string) (dsn, database string, err error) {
	var v string
	switch {
	case s.Op != "":
		cmd := exec.CommandContext(ctx, "op", "read", s.Op)
		cmd.Env = secret.Environ()
		out, err := cmd.Output()
		if err != nil {
			return "", "", fmt.Errorf("op read %s: %w", s.Op, err)
		}
		v = strings.TrimSpace(string(out))
	case s.Env != "":
		if v = os.Getenv(s.Env); v == "" {
			return "", "", fmt.Errorf("environment variable %s is not set", s.Env)
		}
	default:
		if r := s.ref(); !r.IsZero() {
			return r.Read(root, driver, os.LookupEnv)
		}
		return "", "", errors.New("no source")
	}
	dsn, err = appconf.Convert(v, driver)
	return dsn, "", err
}

// ReadDSN reads the DSN of a database component (Secret.Read). When the
// project's setting it reads names a database, that must be working: the
// file is followed when a command runs, working is not, and a restore of
// another database than the one the application runs on would reset the
// wrong one. That error is ErrInvalid and names only the databases. SQL
// Server compares the names in any letter case, as its default collation
// does. SQLite needs no DSN (it returns ""); a setting it names must name
// the file working is.
func (c Component) ReadDSN(ctx context.Context, root string) (string, error) {
	if c.Driver == SQLite && c.DSN.IsZero() {
		return "", nil
	}
	dsn, named, err := c.DSN.read(ctx, root, c.Driver)
	if err != nil {
		return "", err
	}
	if err := c.checkNamed(root, named); err != nil {
		return "", err
	}
	return dsn, nil
}

// checkNamed checks the database a setting names ("" for none) against
// working (ReadDSN).
func (c Component) checkNamed(root, named string) error {
	switch {
	case named == "", named == c.Working, c.Driver == SQLServer && strings.EqualFold(named, c.Working):
		return nil
	case c.Driver == SQLite && sameSQLiteFile(root, named, c.Working):
		return nil
	}
	r := c.DSN.ref()
	where := r.File
	if r.Key != "" {
		where += " " + r.Key
	}
	if c.Driver == SQLite {
		return invalid{fmt.Errorf("dsn: %s names the database file %s, but working is %s: the application runs on %s, and DriftGate would restore %s; set working: %s, or change the setting", where, named, c.Working, named, c.Working, named)}
	}
	return invalid{fmt.Errorf("dsn: %s names the database %s, but working is %s: the application runs on %s, and DriftGate would restore %s; set working: %s (and pristine:), or change the setting", where, named, c.Working, named, c.Working, named)}
}
