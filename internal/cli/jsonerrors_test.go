package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
)

// TestJSONErrorsTemplate: init writes json-errors for a framework that
// renders errors by Accept, not for others, and empty-body for a framework
// whose discovery reads the rules it validates bodies against (a changed
// validation error must not pass unseen); a project of such a framework
// without json-errors gets a note, once per store.
func TestJSONErrorsTemplate(t *testing.T) {
	for files, want := range map[[2]string]string{
		{"composer.json", `{"require":{"symfony/framework-bundle":"7.1"}}`}: "templates: [ok, not-found, json, json-errors]\n",
		{"pyproject.toml", "dependencies = [\"fastapi>=0.110\"]"}:           "templates: [ok, not-found, json, empty-body]\n",
		{"composer.json", `{"require":{"laravel/framework":"^13.0"}}`}:      "templates: [ok, not-found, json, empty-body, json-errors]\n",
	} {
		root := t.TempDir()
		write(t, filepath.Join(root, files[0]), files[1])
		write(t, filepath.Join(root, "artisan"), "<?php\n")
		cfg := filepath.Join(root, "driftgate.yaml")
		if code, out, errOut := runCLI(t, "init", "--config", cfg); code != 0 {
			t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
		}
		if raw, _ := os.ReadFile(cfg); !strings.Contains(string(raw), want) {
			t.Errorf("%s: driftgate.yaml lacks %q:\n%s", files[0], want, raw)
		}
	}

	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, "version: 1\nproject: shop\ntarget:\n  base_url: http://localhost:8000\ndiscover:\n  - adapter: rails\ntemplates: [ok, not-found]\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	p, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	// A check without a store says nothing and makes no store.
	templatesNote(p, &b, true)
	if _, err := os.Stat(p.Layout.Store); b.Len() != 0 || err == nil {
		t.Errorf("check without a store: %q, store made: %v", b.String(), err == nil)
	}
	// Otherwise once, also across commands.
	templatesNote(p, &b, false)
	templatesNote(p, &b, false)
	templatesNote(p, &b, true)
	if strings.Count(b.String(), "note:") != 1 || !strings.Contains(b.String(), "note: rails renders errors as JSON for API clients; add json-errors to templates: in driftgate.yaml") {
		t.Errorf("once: %q", b.String())
	}
	b.Reset()
	os.Remove(filepath.Join(p.Layout.Store, ".note-templates"))
	p.Templates = append(p.Templates, "json-errors")
	templatesNote(p, &b, false)
	if b.Len() != 0 {
		t.Errorf("note with json-errors: %q", b.String())
	}
}

// TestJSONErrorsCount: generate says how many requests json-errors adds.
func TestJSONErrorsCount(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "routes-src.json"), `[{"method":"GET","path":"/api/items/{id}"},{"method":"GET","path":"/api/news"}]`)
	write(t, cfg, "version: 1\nproject: shop\ntarget:\n  base_url: http://localhost:8000\ndiscover:\n  - adapter: command\n    command: [\"cat\", \"routes-src.json\"]\ntemplates: [ok, not-found, json-errors]\nparams:\n  - name: id\n    value: \"1\"\n")
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	runCLI(t, "discover", "--write", "--config", cfg)
	if code, out, errOut := runCLI(t, "generate", "--config", cfg); code != 0 || !strings.Contains(out, "4 generated requests (1 of them json-errors)") {
		t.Fatalf("generate: %d\n%s\n%s", code, out, errOut)
	}
}

// TestTemplatesNoteEmptyBody: a Laravel project made with older templates
// hears of empty-body as well as json-errors (its validation errors are
// otherwise unchecked), and of empty-body alone once it has json-errors.
func TestTemplatesNoteEmptyBody(t *testing.T) {
	for templates, want := range map[string]string{
		"[ok, not-found, json]": "note: laravel renders errors as JSON for API clients, and DriftGate reads laravel's validation rules, so an empty body shows a validation error; add json-errors and empty-body to templates: in driftgate.yaml",
		"[ok, not-found, json, json-errors]": "note: DriftGate reads laravel's validation rules, so an empty body shows a validation error; add empty-body to templates: in driftgate.yaml",
		"[ok, not-found, json, empty-body, json-errors]": "",
	} {
		root := t.TempDir()
		cfg := filepath.Join(root, "driftgate.yaml")
		write(t, cfg, "version: 1\nproject: shop\ntarget:\n  base_url: http://localhost:8000\ndiscover:\n  - adapter: laravel\n    command: [\"php\", \"artisan\", \"route:list\", \"--json\"]\ntemplates: "+templates+"\n")
		t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
		p, err := config.Load(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		templatesNote(p, &b, false)
		if want == "" && b.Len() != 0 || want != "" && !strings.Contains(b.String(), want) {
			t.Errorf("%s: %q, want %q", templates, b.String(), want)
		}
	}
}
