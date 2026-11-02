package generate

import (
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/routes"
)

// withMultipart turns the requests of a route that takes files into
// multipart/form-data requests: the sample body becomes form fields, and
// every file field gets a synthetic file, so no real file is ever named by
// a generated entry. The empty-body template sends an empty form.
func withMultipart(route routes.Route, reqs []catalog.Request) []catalog.Request {
	if len(route.Files) == 0 {
		return reqs
	}
	for i, r := range reqs {
		if len(r.Body) == 0 {
			continue
		}
		mp := &catalog.Multipart{}
		if r.Source.Template != "empty-body" {
			mp.Fields = FormFields(r.Body)
			for _, f := range route.Files {
				mp.Files = append(mp.Files, catalog.File{Field: f.Field, Synthetic: f.Kind})
			}
		}
		var headers [][2]string
		for _, h := range r.Headers {
			if !strings.EqualFold(h[0], "content-type") {
				headers = append(headers, h)
			}
		}
		reqs[i].Headers, reqs[i].Body, reqs[i].Multipart = headers, nil, mp
	}
	return reqs
}

// FormFields writes a JSON object as form fields, the way PHP and most
// frameworks read them back: nested keys in brackets (address[city]), list
// items by index (tags[0]), true as 1 and false as 0, null as empty.
func FormFields(body []byte) [][2]string {
	v, err := jsonx.Parse(body)
	if err != nil || v.Kind != jsonx.Object {
		return nil
	}
	var out [][2]string
	var walk func(name string, v *jsonx.Value)
	walk = func(name string, v *jsonx.Value) {
		switch v.Kind {
		case jsonx.Object:
			for _, m := range v.Members {
				walk(name+"["+m.Key+"]", m.Value)
			}
		case jsonx.Array:
			for i, it := range v.Items {
				walk(name+"["+strconv.Itoa(i)+"]", it)
			}
		case jsonx.Null:
			out = append(out, [2]string{name, ""})
		case jsonx.Bool:
			if v.Bool {
				out = append(out, [2]string{name, "1"})
			} else {
				out = append(out, [2]string{name, "0"})
			}
		default:
			out = append(out, [2]string{name, v.FormatText()})
		}
	}
	for _, m := range v.Members {
		walk(m.Key, m.Value)
	}
	return out
}
