package discover

import (
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/upload"
)

// isFileRule reports the validation rules of a field that takes a file.
func isFileRule(rs []string) bool {
	return hasRule(rs, "file") || hasRule(rs, "image") || hasRule(rs, "mimes") || hasRule(rs, "mimetypes")
}

// withFiles marks a route whose form request requires files: its body is
// sent as multipart/form-data, with a synthetic file of a kind the rules
// accept in each required file field.
func withFiles(r *routes.Route, rules map[string][]string) {
	var fields []string
	for f, rs := range rules {
		if hasRule(rs, "required") && isFileRule(rs) {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return
	}
	sort.Strings(fields)
	r.Files = nil
	for _, f := range fields {
		r.Files = append(r.Files, routes.FileField{Field: formName(f), Kind: fileKind(rules[f])})
	}
	r.BodyType = routes.Multipart
}

// formName writes a dotted rule name as a form field: files.* → files[],
// address.photo → address[photo].
func formName(dotted string) string {
	parts := strings.Split(dotted, ".")
	name := parts[0]
	for _, p := range parts[1:] {
		if p == "*" {
			p = ""
		}
		name += "[" + p + "]"
	}
	return name
}

// fileKind picks the first kind the rules accept: mimes: lists extensions,
// mimetypes: media types; image alone takes a PNG and file alone a text file.
func fileKind(rs []string) string {
	if arg, ok := ruleArg(rs, "mimes"); ok {
		for _, ext := range strings.Split(arg, ",") {
			if k := upload.KindOf(strings.TrimSpace(ext), ""); k != "" {
				return k
			}
		}
	}
	if arg, ok := ruleArg(rs, "mimetypes"); ok {
		for _, mt := range strings.Split(arg, ",") {
			if k := upload.KindOf("", strings.TrimSpace(mt)); k != "" {
				return k
			}
		}
	}
	if hasRule(rs, "image") {
		return upload.PNG
	}
	return upload.TXT
}
