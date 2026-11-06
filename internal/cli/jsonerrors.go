package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/routes"
)

// errorsByAccept are the frameworks (as detect and discover: name them)
// that render an error as a page or a redirect for a browser and as JSON
// for an API client: for them the json-errors template makes a request
// that differs from the error variant it doubles.
var errorsByAccept = []string{"laravel", "symfony", "rails", "spring"}

// templatesFor is the templates: list init writes for the frameworks found.
func templatesFor(kinds []string) string {
	for _, k := range kinds {
		if slices.Contains(errorsByAccept, k) {
			return "[ok, not-found, json, " + routes.JSONErrors + "]"
		}
	}
	return "[ok, not-found, json]"
}

// jsonErrorsNote says, once per local store, that a project of such a
// framework generates no JSON variants of its error variants. The store
// keeps a mark that it was said; a check (generate --check, a run that
// only checks) makes no store for it, it says nothing without one.
func jsonErrorsNote(p *config.Project, out io.Writer, check bool) {
	if slices.Contains(p.Templates, routes.JSONErrors) {
		return
	}
	framework := ""
	for _, a := range p.Discover {
		if slices.Contains(errorsByAccept, a.Adapter) {
			framework = a.Adapter
			break
		}
	}
	if framework == "" {
		return
	}
	if _, err := os.Stat(p.Layout.Store); err != nil {
		if check {
			return
		}
		if _, err := p.WriteStore(); err != nil {
			return
		}
	}
	mark := filepath.Join(p.Layout.Store, ".note-json-errors")
	if _, err := os.Stat(mark); err == nil {
		return
	}
	if os.WriteFile(mark, nil, 0o600) != nil {
		return // said once, or not at all
	}
	fmt.Fprintf(out, "note: %s renders errors as JSON for API clients; add json-errors to templates: in %s and run driftgate generate --write to compare them too\n",
		framework, rel(p.Root, p.Layout.Config))
}

// countTemplate counts the generated requests of a template.
func countTemplate(cat *catalog.Catalog, tpl string) int {
	n := 0
	for _, r := range cat.Requests {
		if r.Source != nil && r.Source.Template == tpl {
			n++
		}
	}
	return n
}
