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
	list := append([]string{"ok", "not-found", "json"}, frameworkTemplates(kinds)...)
	return "[" + strings.Join(list, ", ") + "]"
}

// frameworkTemplates are the templates the frameworks kinds need beyond
// ok, not-found and json: empty-body and json-errors.
func frameworkTemplates(kinds []string) []string {
	var list []string
	if firstOf(kinds, validatesBodies) != "" {
		list = append(list, routes.EmptyBody)
	}
	if firstOf(kinds, errorsByAccept) != "" {
		list = append(list, routes.JSONErrors)
	}
	return list
}

// firstOf is the first of kinds that is one of frameworks, or "".
func firstOf(kinds, frameworks []string) string {
	if i := slices.IndexFunc(kinds, func(k string) bool { return slices.Contains(frameworks, k) }); i >= 0 {
		return kinds[i]
	}
	return ""
}

// templatesNote says, once per local store and template, which of the
// templates its frameworks need (frameworkTemplates of its adapters) a
// project lacks. The store keeps a mark .note-<template> that it was said;
// a check (generate --check, a run that only checks) makes no store for
// it, it says nothing without one.
func templatesNote(p *config.Project, out io.Writer, check bool) {
	kinds := make([]string, len(p.Discover))
	for i, a := range p.Discover {
		kinds[i] = a.Adapter
	}
	var missing []string
	for _, t := range frameworkTemplates(kinds) {
		if !slices.Contains(p.Templates, t) {
			missing = append(missing, t)
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
	why := map[string]string{
		routes.EmptyBody:  firstOf(kinds, validatesBodies) + " validates request bodies (empty-body sends an invalid one, so a changed validation message does not pass unseen)",
		routes.JSONErrors: firstOf(kinds, errorsByAccept) + " renders errors as JSON for API clients",
	}
	var names, reasons []string
	for _, t := range missing {
		mark := filepath.Join(p.Layout.Store, ".note-"+t)
		if _, err := os.Stat(mark); err == nil {
			continue
		}
		if os.WriteFile(mark, nil, 0o600) != nil {
			continue // said once, or not at all
		}
		names, reasons = append(names, t), append(reasons, why[t])
	}
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(out, "note: %s; add %s to templates: in %s and run driftgate generate --write to compare them too\n",
		strings.Join(reasons, "; "), strings.Join(names, " and "), rel(p.Root, p.Layout.Config))
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
