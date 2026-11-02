package discover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/routes"
)

// maxSpec bounds the size of a specification.
const maxSpec = 50 << 20

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// openAPI reads the paths of an OpenAPI 3 or Swagger 2 specification, in JSON
// or YAML. The path prefix comes from the first server (basePath in Swagger
// 2), the need for authentication from security requirements, and a sample
// body from the example of the JSON request body or, failing that, from its
// schema.
func openAPI(raw []byte) ([]routes.Route, error) {
	if len(raw) > maxSpec {
		return nil, fmt.Errorf("specification larger than %d MB", maxSpec>>20)
	}
	// JSON is read as JSON: YAML does not know every JSON escape (PHP writes
	// \/ for /, which API Platform serves).
	var parsed any
	var jsonDoc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' && dec.Decode(&jsonDoc) == nil {
		parsed = fromJSON(jsonDoc)
	} else {
		var node yaml.Node
		if err := yaml.Unmarshal(raw, &node); err != nil {
			return nil, fmt.Errorf("specification: %w", err)
		}
		if hasAlias(&node) {
			// Each alias would be expanded, and a few lines of nested aliases
			// expand to billions of nodes.
			return nil, fmt.Errorf("specification: YAML aliases (*name) are not supported; OpenAPI refers with $ref")
		}
		parsed = plain(&node, 0)
	}
	doc, ok := parsed.(map[string]any)
	if !ok || (doc["paths"] == nil && doc["openapi"] == nil && doc["swagger"] == nil) {
		return nil, fmt.Errorf("not an OpenAPI or Swagger document")
	}
	prefix := ""
	if bp, ok := doc["basePath"].(string); ok {
		prefix = bp
	}
	if servers, ok := doc["servers"].([]any); ok && len(servers) > 0 {
		if s, ok := servers[0].(map[string]any); ok {
			if u, ok := s["url"].(string); ok {
				if parsed, err := url.Parse(u); err == nil {
					prefix = parsed.Path
				}
			}
		}
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("the base path %q does not start with /", prefix)
	}
	_, globalSecurity := doc["security"]
	if list, ok := doc["security"].([]any); ok && len(list) == 0 {
		globalSecurity = false
	}
	paths, _ := doc["paths"].(map[string]any)
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	r := &resolver{doc: doc}
	var out []routes.Route
	for _, p := range names {
		item, _ := r.deref(paths[p]).(map[string]any)
		for _, m := range httpMethods {
			op, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			route := routes.Route{Methods: methodList([]string{m}), Path: prefix + "/" + strings.TrimPrefix(p, "/"), Name: str(op["operationId"])}
			if route.Methods[0] == "HEAD" && item["get"] != nil {
				continue // the GET operation covers it
			}
			auth := globalSecurity
			if sec, ok := op["security"].([]any); ok {
				auth = len(sec) > 0
			}
			route.Auth = auth
			route.Query = r.requiredQuery(item, op)
			if body, ct := r.requestBody(op); body != nil {
				if ct != "application/json" {
					route.BodyType = ct
				}
				b, err := json.Marshal(body)
				if err != nil {
					return nil, err
				}
				route.Body, route.BodySource = b, "openapi"
			}
			if err := r.multipart(op, &route); err != nil {
				return nil, err
			}
			out = append(out, route)
		}
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

type resolver struct{ doc map[string]any }

// deref follows a local $ref (#/components/schemas/X).
func (r *resolver) deref(v any) any {
	for depth := 0; depth < 20; depth++ {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return v
		}
		var cur any = r.doc
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			obj, _ := cur.(map[string]any)
			cur = obj[part]
		}
		v = cur
	}
	return v
}

// requiredQuery gives the required query parameters of an operation (its
// own and those of its path) a sample value.
func (r *resolver) requiredQuery(item, op map[string]any) map[string]string {
	out := map[string]string{}
	for _, src := range []any{item["parameters"], op["parameters"]} {
		list, _ := src.([]any)
		for _, p := range list {
			pm, _ := r.deref(p).(map[string]any)
			if pm["in"] != "query" || pm["required"] != true {
				continue
			}
			var v any
			if ex, ok := pm["example"]; ok {
				v = ex
			} else if s, ok := pm["schema"]; ok {
				v = r.sampleNamed(s, str(pm["name"]), 0)
			} else {
				v = r.sampleNamed(pm, str(pm["name"]), 0) // Swagger 2: the type is on the parameter
			}
			if v != nil {
				out[str(pm["name"])] = fmt.Sprint(v)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// jsonBodyType picks the media type of a request body DriftGate can send:
// application/json, any +json type (application/ld+json,
// application/merge-patch+json), or */*.
func jsonBodyType(content map[string]any) string {
	if _, ok := content["application/json"]; ok {
		return "application/json"
	}
	types := make([]string, 0, len(content))
	for ct := range content {
		types = append(types, ct)
	}
	sort.Strings(types)
	for _, ct := range types {
		if strings.HasSuffix(strings.SplitN(ct, ";", 2)[0], "+json") {
			return ct
		}
	}
	if _, ok := content["*/*"]; ok {
		return "*/*"
	}
	return ""
}

// requestBody returns a sample body and the media type to send it as.
func (r *resolver) requestBody(op map[string]any) (any, string) {
	if rb, ok := r.deref(op["requestBody"]).(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		if ct := jsonBodyType(content); ct != "" {
			mt, _ := content[ct].(map[string]any)
			sendAs := ct
			if ct == "*/*" {
				sendAs = "application/json"
			}
			if ex, ok := mt["example"]; ok {
				return ex, sendAs
			}
			if exs, ok := mt["examples"].(map[string]any); ok && len(exs) > 0 {
				keys := make([]string, 0, len(exs))
				for k := range exs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if e, ok := r.deref(exs[keys[0]]).(map[string]any); ok {
					if v, ok := e["value"]; ok {
						return v, sendAs
					}
				}
			}
			return r.sample(mt["schema"], 0), sendAs
		}
		return nil, ""
	}
	// Swagger 2: a parameter in body.
	params, _ := op["parameters"].([]any)
	for _, p := range params {
		pm, _ := r.deref(p).(map[string]any)
		if pm["in"] == "body" {
			return r.sample(pm["schema"], 0), "application/json"
		}
	}
	return nil, ""
}

// sample builds a value from a schema: its example or default, the first
// enum value, or a value of its type; objects get their required properties.
func (r *resolver) sample(schema any, depth int) any { return r.sampleNamed(schema, "", depth) }

// sampleNamed is sample for a value with a name: a text named email or url
// without a format still gets an e-mail address or a URL.
func (r *resolver) sampleNamed(schema any, name string, depth int) any {
	s, ok := r.deref(schema).(map[string]any)
	if !ok || depth > 8 {
		return nil
	}
	if v, ok := s["example"]; ok {
		return v
	}
	if v, ok := s["default"]; ok {
		return v
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		if list, ok := s[key].([]any); ok && len(list) > 0 {
			if key != "allOf" {
				return r.sample(list[0], depth+1)
			}
			merged := map[string]any{}
			for _, part := range list {
				if m, ok := r.sample(part, depth+1).(map[string]any); ok {
					for k, v := range m {
						merged[k] = v
					}
				}
			}
			return merged
		}
	}
	typ := str(s["type"])
	if list, ok := s["type"].([]any); ok && len(list) > 0 {
		typ = str(list[0]) // OpenAPI 3.1: ["string", "null"]
	}
	if typ == "" && s["properties"] != nil {
		typ = "object"
	}
	switch typ {
	case "object":
		// The required properties; a schema that names none (common in
		// generated specifications) gets all of them.
		out := map[string]any{}
		props, _ := s["properties"].(map[string]any)
		req, _ := s["required"].([]any)
		if len(req) == 0 {
			for name := range props {
				req = append(req, name)
			}
		}
		for _, name := range req {
			n := str(name)
			p, ok := props[n]
			if !ok {
				continue
			}
			if ps, _ := r.deref(p).(map[string]any); ps["readOnly"] == true {
				continue // the server sets it (an id)
			}
			out[n] = r.sampleNamed(p, n, depth+1)
		}
		return out
	case "array":
		if item := r.sample(s["items"], depth+1); item != nil {
			return []any{item}
		}
		return []any{}
	case "integer":
		return 1
	case "number":
		return 1.5
	case "boolean":
		return true
	case "string":
		switch str(s["format"]) {
		case "email":
			return "driftgate@example.test"
		case "date":
			return "2026-01-01"
		case "date-time":
			return "2026-01-01T00:00:00Z"
		case "uuid":
			return "00000000-0000-4000-8000-000000000000"
		case "uri", "url":
			return "https://example.test/driftgate"
		case "decimal", "number", "float", "double":
			return "1.50" // a number written as text (Django REST framework decimals)
		case "int32", "int64", "integer":
			return "1"
		}
		switch lower := strings.ToLower(name); {
		case strings.Contains(lower, "email"):
			return "driftgate@example.test"
		case strings.HasSuffix(lower, "url") || strings.HasSuffix(lower, "uri"):
			return "https://example.test/driftgate"
		}
		return "driftgate"
	}
	return nil
}

// plain turns YAML into plain values the way JSON would read them. Decoding
// straight into any would turn an unquoted date (2026-01-02) into a
// timestamp; here every scalar that is not a number, boolean or null stays
// the text it was written as.
func plain(n *yaml.Node, depth int) any {
	if depth > 100 {
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			return plain(n.Content[0], depth+1)
		}
		return nil
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = plain(n.Content[i+1], depth+1)
		}
		return m
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			list = append(list, plain(c, depth+1))
		}
		return list
	}
	switch n.ShortTag() {
	case "!!int", "!!float":
		var v any
		if n.Decode(&v) == nil {
			return v
		}
	case "!!bool":
		var v bool
		if n.Decode(&v) == nil {
			return v
		}
	case "!!null":
		return nil
	}
	return n.Value
}

// hasAlias reports whether the document uses a YAML alias anywhere.
func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}
	return false
}

// fromJSON turns json.Number into int or float, as YAML decoding would.
func fromJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = fromJSON(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = fromJSON(val)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		f, _ := x.Float64()
		return f
	}
	return v
}
