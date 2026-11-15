package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCommandForms: a command is a list, or {run, dir, stdin}; anything
// else, an unknown key and an empty program are refused when the file is
// read.
func TestCommandForms(t *testing.T) {
	base := "version: 1\nproject: p\n"
	p := func(cfg string) (*Project, error) {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(base+cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	got, err := p(`seed:
  - ["php", "artisan", "migrate"]
  - { run: ["mysql", "shop"], dir: database, stdin: ~/Data/shop.sql }
prepare:
  - run: ["php", "artisan", "cache:clear"]
guards:
  - name: mail
    command: { run: ["sh", "-c", "grep -q log"], stdin: .env }
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{{Run: []string{"php", "artisan", "migrate"}}, {Run: []string{"mysql", "shop"}, Dir: "database", Stdin: "~/Data/shop.sql"}}
	if !reflect.DeepEqual(got.Seed, want) || got.Prepare[0].Run[2] != "cache:clear" || got.Guards[0].Command.Stdin != ".env" {
		t.Fatalf("seed %+v, prepare %+v, guards %+v", got.Seed, got.Prepare, got.Guards)
	}
	for cfg, msg := range map[string]string{
		"seed:\n  - { run: [mysql], shell: true }\n":        `not "shell"`,
		"seed:\n  - { dir: db }\n":                          "must not be empty",
		"prepare:\n  - mysql shop\n":                        "is a list",
		"guards:\n  - name: g\n    command: { stdin: x }\n": "needs name and command",
	} {
		if _, err := p(cfg); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: %v, want %q", cfg, err, msg)
		}
	}
	// Written back, a plain command stays a list.
	raw, err := yaml.Marshal(want)
	if err != nil || !strings.HasPrefix(string(raw), "- - php\n") || !strings.Contains(string(raw), "stdin: ~/Data/shop.sql") {
		t.Fatalf("%v\n%s", err, raw)
	}
}

// TestCommandPaths: dir and stdin are relative to the project root (~/ the
// home folder) and must exist.
func TestCommandPaths(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "db"), 0o755)
	os.WriteFile(filepath.Join(root, "db", "dump.sql"), []byte("x"), 0o644)
	p := &Project{Root: root}
	dir, stdin, err := p.CommandPaths(Command{Run: []string{"mysql"}, Dir: "db", Stdin: "db/dump.sql"})
	if err != nil || dir != filepath.Join(root, "db") || stdin != filepath.Join(root, "db", "dump.sql") {
		t.Fatalf("%q %q %v", dir, stdin, err)
	}
	if dir, stdin, err := p.CommandPaths(Command{Run: []string{"true"}}); err != nil || dir != root || stdin != "" {
		t.Fatalf("defaults: %q %q %v", dir, stdin, err)
	}
	home, _ := os.UserHomeDir()
	if dir, _, err := p.CommandPaths(Command{Run: []string{"true"}, Dir: "~"}); err != nil || dir != home {
		t.Fatalf("dir ~: %q %v", dir, err)
	}
	for c, msg := range map[*Command]string{
		{Run: []string{"mysql"}, Stdin: "missing.sql"}: "stdin " + pathText(filepath.Join(root, "missing.sql")) + " does not exist",
		{Run: []string{"mysql"}, Stdin: "db"}:          "is a folder, not a file",
		{Run: []string{"mysql"}, Dir: "nowhere"}:       "is not a folder",
		// The message names the command with its secrets hidden.
		{Run: []string{"mysql", "-ps3cr"}, Stdin: "missing.sql"}:             "mysql -p***: stdin ",
		{Run: []string{"sh", "-c", "mysql --password=s3cr"}, Dir: "nowhere"}: `sh -c "mysql --password=***": dir `,
	} {
		if _, _, err := p.CommandPaths(*c); err == nil || !strings.Contains(err.Error(), msg) || strings.Contains(err.Error(), "s3cr") {
			t.Errorf("%+v: %v, want %q", *c, err, msg)
		}
	}
}
