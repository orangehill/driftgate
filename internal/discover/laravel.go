package discover

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/routes"
)

//go:embed laravel_rules.php
var rulesScript []byte

// laravel reads `php artisan route:list`: its JSON output (--json) or the
// table older versions print. Notices printed before the JSON are skipped.
// The rules script runs at the same time (each boots the application); what
// it prints to the log follows that of route:list, as when they ran one
// after the other.
func laravel(ctx context.Context, root string, a Adapter, log io.Writer) ([]routes.Route, error) {
	var rules struct {
		out, log []byte
		err      error
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); <-done }()
	if len(a.PHP) > 0 {
		go func() {
			defer close(done)
			var buf bytes.Buffer
			rules.out, rules.err = runCommand(ctx, root, a.PHP, rulesScript, &buf)
			rules.log = buf.Bytes()
		}()
	} else {
		close(done)
	}
	out, err := runCommand(ctx, root, a.Command, nil, log)
	if err != nil {
		return nil, err
	}
	var rs []routes.Route
	if js := jsonStart(out); js != nil {
		rs, err = laravelJSON(js)
	} else {
		rs, err = laravelTable(string(out))
	}
	if err != nil {
		return nil, err
	}
	if len(a.PHP) == 0 {
		return rs, nil
	}
	<-done
	log.Write(rules.log)
	if rules.err != nil {
		return nil, fmt.Errorf("reading validation rules: %w", rules.err)
	}
	return withRules(rs, rules.out)
}

// jsonStart finds a JSON list that starts a line, or nil.
func jsonStart(out []byte) []byte {
	trimmed := bytes.TrimSpace(out)
	if bytes.HasPrefix(trimmed, []byte("[")) {
		return trimmed
	}
	if i := bytes.Index(out, []byte("\n[")); i >= 0 {
		return bytes.TrimSpace(out[i+1:])
	}
	return nil
}

func laravelRoute(domain, method, uri, name, action string, middleware []string) routes.Route {
	path := "/" + strings.Trim(uri, "/")
	return routes.Route{Methods: methodList([]string{method}), Path: path, Domain: domain, Name: name,
		Action: action, Middleware: middleware, Auth: needsAuth(middleware)}
}

func laravelJSON(out []byte) ([]routes.Route, error) {
	var list []struct {
		Domain     *string         `json:"domain"`
		Method     string          `json:"method"`
		URI        string          `json:"uri"`
		Name       *string         `json:"name"`
		Action     string          `json:"action"`
		Middleware json.RawMessage `json:"middleware"`
		Path       *string         `json:"path"` // newer versions: file and line of a closure
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("route:list JSON: %w", err)
	}
	var rs []routes.Route
	for _, e := range list {
		var mw []string
		if err := json.Unmarshal(e.Middleware, &mw); err != nil {
			var s string
			json.Unmarshal(e.Middleware, &s)
			mw = splitMiddleware(s)
		}
		domain, name := "", ""
		if e.Domain != nil {
			domain = *e.Domain
		}
		if e.Name != nil {
			name = *e.Name
		}
		r := laravelRoute(domain, e.Method, e.URI, name, e.Action, mw)
		if e.Path != nil && !filepath.IsAbs(*e.Path) {
			r.File = filepath.ToSlash(*e.Path)
		}
		rs = append(rs, r)
	}
	return rs, nil
}

func splitMiddleware(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// laravelTable reads the table of route:list (Laravel 5.x):
//
//	| Domain | Method   | URI       | Name | Action          | Middleware |
//	|        | GET|HEAD | api/news  |      | App\…@index     | api        |
//
// A row with an empty method continues the middleware of the row above.
func laravelTable(out string) ([]routes.Route, error) {
	var cols map[string]int
	var rs []routes.Route
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cols == nil {
			cols = map[string]int{}
			for i, c := range cells {
				cols[strings.ToLower(c)] = i
			}
			if _, ok := cols["uri"]; !ok {
				return nil, fmt.Errorf("route:list table: no URI column in %q", line)
			}
			continue
		}
		// The method column itself holds "GET|HEAD"; split it back.
		if len(cells) > len(cols) {
			mi := cols["method"]
			extra := len(cells) - len(cols)
			cells = append(cells[:mi], append([]string{strings.Join(cells[mi:mi+extra+1], "|")}, cells[mi+extra+1:]...)...)
		}
		cell := func(name string) string {
			if i, ok := cols[name]; ok && i < len(cells) {
				return cells[i]
			}
			return ""
		}
		if cell("method") == "" && cell("uri") == "" {
			if len(rs) > 0 {
				last := &rs[len(rs)-1]
				last.Middleware = append(last.Middleware, splitMiddleware(cell("middleware"))...)
				last.Auth = needsAuth(last.Middleware)
			}
			continue
		}
		rs = append(rs, laravelRoute(cell("domain"), cell("method"), cell("uri"), cell("name"), cell("action"), splitMiddleware(cell("middleware"))))
	}
	if cols == nil {
		return nil, fmt.Errorf("route:list printed neither JSON nor a table")
	}
	return rs, nil
}

// withRules attaches the source file of each action and a sample body built
// from the validation rules that the PHP script printed.
func withRules(rs []routes.Route, out []byte) ([]routes.Route, error) {
	const marker = "__DRIFTGATE_RULES__"
	i := bytes.LastIndex(out, []byte(marker))
	if i < 0 {
		return nil, fmt.Errorf("reading validation rules: the PHP script printed no result")
	}
	var list []struct {
		Methods []string            `json:"methods"`
		URI     string              `json:"uri"`
		Domain  *string             `json:"domain"`
		File    string              `json:"file"`
		Rules   map[string][]string `json:"rules"` // nil: no form request
		Error   *string             `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(out[i+len(marker):])).Decode(&list); err != nil {
		return nil, fmt.Errorf("reading validation rules: %w", err)
	}
	for _, e := range list {
		domain := ""
		if e.Domain != nil {
			domain = *e.Domain
		}
		path := "/" + strings.Trim(e.URI, "/")
		methods := strings.Join(methodList(e.Methods), "|")
		for j := range rs {
			if rs[j].Path != path || rs[j].Domain != domain || strings.Join(rs[j].Methods, "|") != methods {
				continue
			}
			if e.File != "" {
				rs[j].File = e.File
			}
			if e.Error != nil {
				rs[j].BodySource = "validation rules could not be read: " + *e.Error
				continue
			}
			if e.Rules == nil {
				continue
			}
			body, err := json.Marshal(sampleFromRules(e.Rules))
			if err != nil {
				return nil, err
			}
			rs[j].Body, rs[j].BodySource = body, "validation rules"
			withFiles(&rs[j], e.Rules)
		}
	}
	return rs, nil
}

var dateFormat = strings.NewReplacer("Y", "2026", "y", "26", "m", "01", "n", "1", "d", "01", "j", "1", "H", "00", "G", "0", "i", "00", "s", "00")

// sampleFromRules builds a body that passes the usual rules of the required
// fields: a value of the right type, size and format for each. Fields that
// are not required are left out, so a rule like exists: on an optional field
// does not decide the outcome. Dotted names (address.city, tags.*) nest.
func sampleFromRules(rules map[string][]string) map[string]any {
	body := map[string]any{}
	fields := make([]string, 0, len(rules))
	for f, rs := range rules {
		if hasRule(rs, "required") {
			fields = append(fields, f)
		}
	}
	sort.Strings(fields)
	for _, f := range fields {
		rs := rules[f]
		if isFileRule(rs) {
			continue // a JSON body cannot carry a file
		}
		v := sampleValue(rs)
		put(body, strings.Split(f, "."), v)
		if hasRule(rs, "confirmed") {
			put(body, strings.Split(f+"_confirmation", "."), v)
		}
	}
	return body
}

func put(obj map[string]any, path []string, v any) {
	key := path[0]
	if len(path) == 1 {
		obj[key] = v
		return
	}
	if path[1] == "*" {
		list, _ := obj[key].([]any)
		if len(path) == 2 {
			obj[key] = append(list, v)
			return
		}
		var item map[string]any
		if len(list) > 0 {
			item, _ = list[0].(map[string]any)
		}
		if item == nil {
			item = map[string]any{}
			list = append(list, item)
		}
		put(item, path[2:], v)
		obj[key] = list
		return
	}
	child, _ := obj[key].(map[string]any)
	if child == nil {
		child = map[string]any{}
		obj[key] = child
	}
	put(child, path[1:], v)
}

func ruleArg(rs []string, name string) (string, bool) {
	for _, r := range rs {
		n, arg, _ := strings.Cut(r, ":")
		if n == name {
			return arg, true
		}
	}
	return "", false
}

func hasRule(rs []string, name string) bool {
	_, ok := ruleArg(rs, name)
	return ok
}

var digitsRE = lazy.Regexp(`^\d+$`)

func sampleValue(rs []string) any {
	if arg, ok := ruleArg(rs, "in"); ok {
		first := strings.Trim(strings.Split(arg, ",")[0], `"'`)
		if digitsRE().MatchString(first) && (hasRule(rs, "integer") || hasRule(rs, "numeric")) {
			n, _ := strconv.Atoi(first)
			return n
		}
		return first
	}
	min := 0
	if arg, ok := ruleArg(rs, "min"); ok {
		min, _ = strconv.Atoi(arg)
	}
	if arg, ok := ruleArg(rs, "between"); ok {
		min, _ = strconv.Atoi(strings.Split(arg, ",")[0])
	}
	if arg, ok := ruleArg(rs, "size"); ok {
		min, _ = strconv.Atoi(arg)
	}
	switch {
	case hasRule(rs, "boolean") || hasRule(rs, "accepted"):
		return true
	case hasRule(rs, "integer") || hasRule(rs, "numeric"):
		if min > 1 {
			return min
		}
		return 1
	case hasRule(rs, "array"):
		return []any{}
	case hasRule(rs, "email"):
		return "driftgate@example.test"
	case hasRule(rs, "url"):
		return "https://example.test/driftgate"
	case hasRule(rs, "uuid"):
		return "00000000-0000-4000-8000-000000000000"
	case hasRule(rs, "json"):
		return "{}"
	case hasRule(rs, "date_format"):
		f, _ := ruleArg(rs, "date_format")
		return dateFormat.Replace(f)
	case hasRule(rs, "date"):
		return "2026-01-01"
	}
	s := "driftgate"
	for len(s) < min {
		s += "-driftgate"
	}
	if min > 0 && len(s) > min && (hasRule(rs, "size") || hasRule(rs, "max")) {
		s = s[:min]
	}
	if arg, ok := ruleArg(rs, "max"); ok {
		if n, _ := strconv.Atoi(arg); n > 0 && len(s) > n {
			s = s[:n]
		}
	}
	return s
}
