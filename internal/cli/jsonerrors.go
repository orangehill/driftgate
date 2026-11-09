package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/routes"
)

// errorsByAccept are the frameworks (as detect and discover: name them)
// that render an error as a page or a redirect for a browser and as JSON
// for an API client: for them the json-errors template makes a request
// that differs from the error variant it doubles.
var errorsByAccept = []string{"laravel", "symfony", "rails", "spring"}

// validatesBodies are the frameworks (as detect names them) whose
// discovery reads the rules the framework itself checks every request body
// against: Laravel's validation rules and FastAPI's models (in its OpenAPI
// document). An empty body answers a validation error there, so init
// writes the empty-body template for them: without a request that sends an
// invalid body, a changed validation message would pass unseen. generate
// makes empty-body requests only for routes with a sample body (rules
// found), so a route without rules gets none.
var validatesBodies = []string{"laravel", "fastapi"}

// templatesFor is the templates: list init writes for the frameworks found.
func templatesFor(kinds []string) string {
	list := []string{"ok", "not-found", "json"}
	if slices.ContainsFunc(kinds, func(k string) bool { return slices.Contains(validatesBodies, k) }) {
		list = append(list, "empty-body")
	}
	if slices.ContainsFunc(kinds, func(k string) bool { return slices.Contains(errorsByAccept, k) }) {
		list = append(list, routes.JSONErrors)
	}
	return "[" + strings.Join(list, ", ") + "]"
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
