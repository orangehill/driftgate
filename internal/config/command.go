package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/orangehill/driftgate/internal/procx"
	"gopkg.in/yaml.v3"
)

// Command is a command of the project (seed, prepare, a guard): the
// program and its arguments, run without a shell. YAML takes the list
// (["php", "artisan", "migrate"]) or a mapping that also names the folder
// it runs in and a file for its standard input:
// {run: ["mysql", "shop"], dir: database, stdin: ~/Data/shop.sql}.
type Command struct {
	Run   []string `yaml:"run"`
	Dir   string   `yaml:"dir,omitempty"`   // where it runs; default the project root
	Stdin string   `yaml:"stdin,omitempty"` // a file given as its standard input
}

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		*c = Command{}
		return n.Decode(&c.Run)
	case yaml.MappingNode:
		// A node decodes without the file's KnownFields: unknown keys are
		// refused here.
		for i := 0; i < len(n.Content); i += 2 {
			switch k := n.Content[i].Value; k {
			case "run", "dir", "stdin":
			default:
				return fmt.Errorf("line %d: a command takes run, dir and stdin, not %q", n.Content[i].Line, k)
			}
		}
		type plain Command
		var v plain
		if err := n.Decode(&v); err != nil {
			return err
		}
		*c = Command(v)
		return nil
	}
	return fmt.Errorf("line %d: a command is a list (the program and its arguments) or {run: [...], dir: …, stdin: …}", n.Line)
}

func (c Command) MarshalYAML() (any, error) {
	if c.Dir == "" && c.Stdin == "" {
		return c.Run, nil
	}
	type plain Command
	return plain(c), nil
}

// String is the command as one line, for messages and logs: its program
// and arguments, then < its stdin file and (in its dir), every secret in
// them shown as *** (procx.Line). A path is a word of its own: what stands
// before it never makes it a secret's value.
func (c Command) String() string {
	s := procx.Line(c.Run)
	if c.Stdin != "" {
		s += " < " + pathText(c.Stdin)
	}
	if c.Dir != "" {
		s += " (in " + pathText(c.Dir) + ")"
	}
	return s
}

// pathText is a path of a command for a message, quoted when it needs to
// be, its secrets shown as *** (procx.Line).
func pathText(path string) string { return procx.Line([]string{path}) }

func (c Command) validate() error {
	if len(c.Run) == 0 || c.Run[0] == "" {
		return errors.New("the program is empty")
	}
	return nil
}

// Resolve resolves a path of the configuration: ~ and ~/ are the home
// folder, a relative path is relative to the project root.
func (p *Project) Resolve(path string) (string, error) {
	if path == "~" {
		return os.UserHomeDir()
	}
	if _, home := HomeRelative(path); !filepath.IsAbs(path) && !home {
		path = filepath.Join(p.Root, path)
	}
	return expand(path)
}

// CommandPaths returns the folder a command runs in and the file of its
// standard input ("" for none), resolved (Resolve). Both must exist: a
// missing one is an error naming it.
func (p *Project) CommandPaths(c Command) (dir, stdin string, err error) {
	dir = p.Root
	if c.Dir != "" {
		if dir, err = p.Resolve(c.Dir); err != nil {
			return "", "", err
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return "", "", fmt.Errorf("%s: dir %s is not a folder", procx.Line(c.Run), pathText(dir))
		}
	}
	if c.Stdin != "" {
		if stdin, err = p.Resolve(c.Stdin); err != nil {
			return "", "", err
		}
		if fi, err := os.Stat(stdin); err != nil {
			return "", "", fmt.Errorf("%s: stdin %s does not exist", procx.Line(c.Run), pathText(stdin))
		} else if fi.IsDir() {
			return "", "", fmt.Errorf("%s: stdin %s is a folder, not a file", procx.Line(c.Run), pathText(stdin))
		}
	}
	return dir, stdin, nil
}
