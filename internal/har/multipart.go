package har

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/upload"
)

// harParam is a posted form field or file (HAR 1.2 postData.params).
type harParam struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
}

func isMultipart(mimeType string) bool {
	mt, _, _ := mime.ParseMediaType(mimeType)
	return mt == "multipart/form-data"
}

func withoutContentType(headers [][2]string) [][2]string {
	var out [][2]string
	for _, h := range headers {
		if h[0] != "content-type" {
			out = append(out, h)
		}
	}
	return out
}

// formCandidate reads a multipart body from the params the browser listed,
// or else from the text of the body. Fields are kept; a file becomes a
// synthetic file of the same kind, under a neutral name: neither the
// uploaded bytes nor the original file name (which may name a person) enter
// the catalog. It returns why the request is left out, if it is.
func formCandidate(mimeType, text string, params []harParam) (*catalog.Multipart, string) {
	if len(params) == 0 && text != "" {
		var err error
		if params, err = parseForm(mimeType, text); err != nil {
			return nil, "multipart body could not be read: " + err.Error() + "; add the request by hand"
		}
	}
	mp := &catalog.Multipart{}
	var values []string
	for _, p := range params {
		if secretName.MatchString(p.Name) {
			return nil, "the form carries " + p.Name + "; add the request by hand"
		}
		if p.FileName == "" && secretValue(p.Value) {
			return nil, "the form carries a token in " + p.Name + "; add the request by hand"
		}
		if p.FileName == "" && p.ContentType == "" {
			mp.Fields = append(mp.Fields, [2]string{p.Name, p.Value})
			values = append(values, p.Value)
			continue
		}
		kind := upload.KindOf(p.FileName, p.ContentType)
		if kind == "" {
			kind = upload.TXT
		}
		mp.Files = append(mp.Files, catalog.File{Field: p.Name, Synthetic: kind})
	}
	if f := detector.Scan([]byte(strings.Join(values, " "))); len(f) > 0 {
		return nil, "the form holds personal data (" + f[0].Kind + "); add the request by hand"
	}
	return mp, ""
}

// parseForm lists the parts of a multipart body; the content of a file part
// is skipped, not read.
func parseForm(mimeType, text string) ([]harParam, error) {
	_, ps, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return nil, err
	}
	if ps["boundary"] == "" {
		return nil, errors.New("no boundary")
	}
	r := multipart.NewReader(strings.NewReader(text), ps["boundary"])
	var out []harParam
	for {
		part, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		p := harParam{Name: part.FormName(), FileName: part.FileName(), ContentType: part.Header.Get("Content-Type")}
		if p.FileName == "" {
			var b bytes.Buffer
			if _, err := io.Copy(&b, io.LimitReader(part, 1<<20)); err != nil {
				return nil, err
			}
			p.Value = b.String()
		}
		out = append(out, p)
	}
}
