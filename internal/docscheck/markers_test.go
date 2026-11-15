package docscheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/discover"
	"github.com/orangehill/driftgate/internal/routes"
)

// TestCheckMarkers: every yaml and json block of every page has a check
// marker on the line before it, and the block passes the loader the marker
// names.
func TestCheckMarkers(t *testing.T) {
	p, files := pages(t)
	for _, f := range files {
		d, err := p.Get(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range d.Markers {
			t.Errorf("%s:%d: check marker %q is not followed by a fenced block", f, m.Line, m.Text)
		}
		for _, b := range d.Blocks {
			isData := b.Lang == "yaml" || b.Lang == "yml" || b.Lang == "json"
			switch {
			case b.Marker == "" && isData:
				t.Errorf("%s:%d: %s block without a <!-- check: … --> marker on the line before", f, b.Line, b.Lang)
			case b.Marker != "" && !isData:
				t.Errorf("%s:%d: check marker %q on a %q block; markers are for yaml and json", f, b.Line, b.Marker, b.Lang)
			case b.Marker != "":
				if err := checkBlock(t.TempDir(), b.Marker, b.Body); err != nil {
					t.Errorf("%s:%d: check: %s: %v", f, b.Line, b.Marker, err)
				}
			}
		}
	}
}

// checkBlock loads body with the loader of the marker: "KIND [files=a,b]".
func checkBlock(dir, mark, body string) error {
	kind, args, _ := strings.Cut(mark, " ")
	args = strings.TrimSpace(args)
	if kind == "skip:" {
		if args == "" {
			return errors.New("skip needs a reason")
		}
		return nil
	}
	var files []string
	if args != "" {
		list, ok := strings.CutPrefix(args, "files=")
		if !ok || kind != "config-fragment" {
			return fmt.Errorf("unknown argument %q", args)
		}
		files = strings.Split(list, ",")
	}
	write := func(name, content string) (string, error) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		return p, os.WriteFile(p, []byte(content), 0o644)
	}
	raw := []byte(body + "\n")
	switch kind {
	case "config", "config-fragment":
		if kind == "config-fragment" {
			body = fragment(body)
			for _, f := range files {
				if _, err := write(f, ""); err != nil {
					return err
				}
			}
		}
		p, err := write(".driftgate/driftgate.yaml", body+"\n")
		if err != nil {
			return err
		}
		_, err = config.Load(p)
		return err
	case "catalog":
		p, err := write("catalog.json", body)
		if err != nil {
			return err
		}
		_, err = catalog.Load(p)
		return err
	case "allowlist":
		_, err := allowlist.Parse(raw)
		return err
	case "exclusions":
		p, err := write("exclusions.yaml", body)
		if err != nil {
			return err
		}
		_, err = routes.LoadExclusions(p)
		return err
	case "classify":
		_, err := data.ParseRules(raw)
		return err
	case "routes-json":
		_, err := discover.ParseCommandOutput(raw)
		return err
	case "yaml":
		// Every document of the block, not only the first.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var v any
			if err := dec.Decode(&v); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
		}
	case "json":
		var v any
		return json.Unmarshal(raw, &v)
	}
	return fmt.Errorf("unknown kind %q", kind)
}

var topKey = regexp.MustCompile(`(?m)^(version|project|target|targets):`)

// fragment completes a part of driftgate.yaml into a file Load reads:
// version, project and a target when the part has none.
func fragment(body string) string {
	has := map[string]bool{}
	for _, m := range topKey.FindAllStringSubmatch(body, -1) {
		has[m[1]] = true
	}
	var head []string
	if !has["version"] {
		head = append(head, "version: 1")
	}
	if !has["project"] {
		head = append(head, "project: shop")
	}
	if !has["target"] && !has["targets"] {
		head = append(head, "target: {base_url: http://localhost:8000}")
	}
	return strings.Join(append(head, body), "\n")
}

// TestCheckBlockFails: a block a loader refuses, and an unknown kind, fail.
func TestCheckBlockFails(t *testing.T) {
	for _, c := range []struct{ mark, body string }{
		{"config-fragment", "mode: sometimes"},
		{"config-fragment", "no_such_key: 1"},
		{"catalog", `{"version": 1, "requests": [{"id": "a", "method": "GET", "path": "x"}]}`},
		{"allowlist", "version: 1\nheaders:\n  - name: Date"},
		{"exclusions", "version: 1\nroutes:\n  - route: GET /x"},
		{"classify", "version: 1\ntabels: {}"},
		{"routes-json", `[{"path": "/x"}]`},
		{"routes-json", `{"path": "/x", "method": "GET"}`},
		{"yaml", "a: 1\n---\nb: [1"},
		{"yaml", "a: [1"},
		{"json", "{"},
		{"skip:", "x"},
		{"configuration", "version: 1"},
		{"catalog files=a", "{}"},
	} {
		if err := checkBlock(t.TempDir(), c.mark, c.body); err == nil {
			t.Errorf("%s accepts %q", c.mark, c.body)
		}
	}
}

// TestIndentedBlock: a block in a list item is checked without the
// indentation of its fence.
func TestIndentedBlock(t *testing.T) {
	d, err := ParseText("x.md", "- item\n\n  <!-- check: config-fragment -->\n  ```yaml\n  mode: evolve\n  level: values\n  ```\n")
	if err != nil || len(d.Blocks) != 1 {
		t.Fatalf("%v %+v", err, d)
	}
	if err := checkBlock(t.TempDir(), d.Blocks[0].Marker, d.Blocks[0].Body); err != nil {
		t.Error(err)
	}
}
