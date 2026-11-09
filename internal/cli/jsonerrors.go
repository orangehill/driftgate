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

// templatesNote says, once per local store, which error templates a
// project of such a framework lacks: json-errors where errors are rendered
// by Accept (no JSON variants of its error variants), and empty-body where
// discovery reads the validation rules (no request shows a validation
// error). The store keeps a mark that it was said; a check (generate
// --check, a run that only checks) makes no store for it, it says nothing
// without one. (A store marked by an earlier release, which named only
// json-errors, hears the whole note once.)
func templatesNote(p *config.Project, out io.Writer, check bool) {
	var missing, does []string
	for _, a := range p.Discover {
		if slices.Contains(errorsByAccept, a.Adapter) && !slices.Contains(p.Templates, routes.JSONErrors) && !slices.Contains(missing, routes.JSONErrors) {
			missing = append(missing, routes.JSONErrors)
			does = append(does, a.Adapter+" renders errors as JSON for API clients")
		}
		if slices.Contains(validatesBodies, a.Adapter) && !slices.Contains(p.Templates, routes.EmptyBody) && !slices.Contains(missing, routes.EmptyBody) {
			missing = append(missing, routes.EmptyBody)
			does = append(does, "DriftGate reads "+a.Adapter+"'s validation rules, so an empty body shows a validation error")
		}
	}
	if len(missing) == 0 {
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
	mark := filepath.Join(p.Layout.Store, ".note-templates")
	if _, err := os.Stat(mark); err == nil {
		return
	}
	if os.WriteFile(mark, nil, 0o600) != nil {
		return // said once, or not at all
	}
	fmt.Fprintf(out, "note: %s; add %s to templates: in %s and run driftgate generate --write to compare them too\n",
		strings.Join(does, ", and "), strings.Join(missing, " and "), rel(p.Root, p.Layout.Config))
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
