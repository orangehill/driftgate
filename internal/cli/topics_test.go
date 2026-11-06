package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/routes"
)

// TestHelpTopics: help catalog, allowlist, exclusions, classify and
// config list every field of those files and the section to read (help allowlist also the allowlist
// command); an unknown topic points to the table of what to use for a task.
func TestHelpTopics(t *testing.T) {
	fields := func(v any, tag string) []string {
		var out []string
		ty := reflect.TypeOf(v)
		for i := range ty.NumField() {
			if name, _, _ := strings.Cut(ty.Field(i).Tag.Get(tag), ","); name != "" && name != "-" {
				out = append(out, name)
			}
		}
		return out
	}
	// The names of the first column, split at commas.
	names := func(out string) map[string]bool {
		m := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
				continue
			}
			first, _, _ := strings.Cut(strings.TrimSpace(line), "  ")
			for _, n := range strings.Split(first, ",") {
				m[strings.TrimSuffix(strings.TrimSpace(n), ":")] = true
			}
		}
		return m
	}
	var multipart catalog.Multipart
	for _, c := range []struct {
		topic string
		keys  []string
		want  []string
	}{
		{"catalog", append(append(fields(catalog.Request{}, "json"), fields(catalog.Catalog{}, "json")...), fields(multipart, "json")...),
			[]string{"(beside driftgate.yaml", docsBase() + "reference/catalog-json.md#request-fields", docsBase() + tasksDoc}},
		{"allowlist", []string{"headers", "body", "arrays", "version", "request", "target", "reason"},
			[]string{"(beside driftgate.yaml)", docsBase() + "reference/allowlist-yaml.md", "usage: driftgate allowlist check"}},
		{"exclusions", append(append(fields(routes.Exclusion{}, "yaml"), fields(routes.Exclusions{}, "yaml")...), fields(routes.KnownFailure{}, "yaml")...),
			[]string{"(beside driftgate.yaml)", docsBase() + "reference/exclusions-yaml.md"}},
		{"classify", append(append(append(fields(data.Rules{}, "yaml"), fields(data.TableRule{}, "yaml")...), fields(data.ColumnRule{}, "yaml")...), fields(data.SchemaRule{}, "yaml")...),
			[]string{"(beside driftgate.yaml)", docsBase() + "reference/classify-yaml.md"}},
		{"config", fields(config.Config{}, "yaml"),
			[]string{"driftgate.yaml", docsBase() + "reference/driftgate-yaml.md#top-level-keys"}},
	} {
		code, out, errOut := runCLI(t, "help", c.topic)
		if code != ExitClean || errOut != "" {
			t.Fatalf("help %s: exit %d %s", c.topic, code, errOut)
		}
		listed := names(out)
		for _, k := range c.keys {
			if !listed[k] {
				t.Errorf("help %s does not list %q in its first column:\n%s", c.topic, k, out)
			}
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("help %s lacks %q:\n%s", c.topic, w, out)
			}
		}
	}
	// The keys of every allowlist rule are named on the line of its kind.
	_, out, _ := runCLI(t, "help", "allowlist")
	for _, k := range append(append(fields(allowlist.HeaderRule{}, "yaml"), fields(allowlist.BodyRule{}, "yaml")...), fields(allowlist.ArrayRule{}, "yaml")...) {
		if !strings.Contains(out, k) {
			t.Errorf("help allowlist does not name %s", k)
		}
	}
	for _, args := range [][]string{{"help", "uploads"}, {"uploads"}} {
		if code, _, errOut := runCLI(t, args...); code != ExitConfig || !strings.Contains(errOut, docsBase()+tasksDoc) {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
}
