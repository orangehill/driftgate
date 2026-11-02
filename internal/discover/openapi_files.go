package discover

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/upload"
)

// multipart describes an operation that takes multipart/form-data (and no
// JSON body, which DriftGate prefers): the fields get sample values, and a
// property of format binary (or base64), or a Swagger 2 parameter of type
// file, gets a synthetic file of the kind its encoding or name suggests.
func (r *resolver) multipart(op map[string]any, route *routes.Route) error {
	if route.Body != nil {
		return nil
	}
	fields := map[string]any{}
	var files []routes.FileField
	if rb, ok := r.deref(op["requestBody"]).(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		mt, ok := content[routes.Multipart].(map[string]any)
		if !ok {
			return nil
		}
		schema, _ := r.deref(mt["schema"]).(map[string]any)
		encoding, _ := mt["encoding"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		req, _ := schema["required"].([]any)
		if len(req) == 0 {
			for name := range props {
				req = append(req, name)
			}
		}
		for _, n := range req {
			name := str(n)
			p, ok := props[name]
			if !ok {
				continue
			}
			ps, _ := r.deref(p).(map[string]any)
			if items, _ := r.deref(ps["items"]).(map[string]any); str(ps["type"]) == "array" && isBinary(items) {
				ps = items
			}
			if !isBinary(ps) {
				fields[name] = r.sampleNamed(p, name, 1)
				continue
			}
			ct := ""
			if e, ok := encoding[name].(map[string]any); ok {
				ct = strings.TrimSpace(strings.Split(str(e["contentType"]), ",")[0])
			}
			if ct == "" {
				ct = str(ps["contentMediaType"])
			}
			files = append(files, routes.FileField{Field: name, Kind: kindFor(name, ct)})
		}
	} else {
		// Swagger 2: formData parameters, a file among them.
		params, _ := op["parameters"].([]any)
		for _, p := range params {
			pm, _ := r.deref(p).(map[string]any)
			if pm["in"] != "formData" || (pm["required"] != true && pm["type"] != "file") {
				continue
			}
			name := str(pm["name"])
			if pm["type"] == "file" {
				files = append(files, routes.FileField{Field: name, Kind: kindFor(name, "")})
				continue
			}
			fields[name] = r.sampleNamed(pm, name, 1)
		}
	}
	if len(files) == 0 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Field < files[j].Field })
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	route.Body, route.BodySource, route.BodyType, route.Files = b, "openapi", routes.Multipart, files
	return nil
}

func isBinary(s map[string]any) bool {
	if str(s["type"]) != "string" {
		return false
	}
	f := str(s["format"])
	return f == "binary" || f == "base64" || s["contentMediaType"] != nil // OpenAPI 3.1 writes binary content by its media type
}

// kindFor picks a synthetic kind from a media type, else from the name of
// the field (avatar, photo, csv, excel…); a text file is the default.
func kindFor(name, mediaType string) string {
	if k := upload.KindOf("", mediaType); k != "" {
		return k
	}
	lower := strings.ToLower(name)
	for _, c := range []struct{ word, kind string }{
		{"image", upload.PNG}, {"photo", upload.PNG}, {"avatar", upload.PNG}, {"logo", upload.PNG}, {"picture", upload.PNG},
		{"csv", upload.CSV}, {"excel", upload.XLSX}, {"xlsx", upload.XLSX}, {"spreadsheet", upload.XLSX}, {"pdf", upload.PDF},
	} {
		if strings.Contains(lower, c.word) {
			return c.kind
		}
	}
	return upload.TXT
}
