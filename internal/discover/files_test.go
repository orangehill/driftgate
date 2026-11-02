package discover

import (
	"reflect"
	"testing"

	"github.com/orangehill/driftgate/internal/routes"
)

func TestLaravelFileRules(t *testing.T) {
	rs := []routes.Route{{Methods: []string{"POST"}, Path: "/api/imports"}, {Methods: []string{"POST"}, Path: "/api/faq"}}
	out := []byte(`noise
__DRIFTGATE_RULES__[
 {"methods":["POST"],"uri":"api/imports","domain":null,"file":"","rules":{"file":["required","file","mimes:xls,xlsx","max:10240"],"kind":["required","in:orders,returns"],"photos.*":["required","image"],"scan":["required","mimetypes:application/pdf"],"note":["nullable","file"]},"error":null},
 {"methods":["POST"],"uri":"api/faq","domain":null,"file":"","rules":{"q":["required","string"]},"error":null}]`)
	got, err := withRules(rs, out)
	if err != nil {
		t.Fatal(err)
	}
	imp := got[0]
	want := []routes.FileField{{Field: "file", Kind: "xlsx"}, {Field: "photos[]", Kind: "png"}, {Field: "scan", Kind: "pdf"}}
	if imp.BodyType != routes.Multipart || !reflect.DeepEqual(imp.Files, want) || string(imp.Body) != `{"kind":"orders"}` {
		t.Fatalf("imports: %s %+v %s", imp.BodyType, imp.Files, imp.Body)
	}
	if got[1].BodyType != "" || got[1].Files != nil {
		t.Fatalf("faq: %+v", got[1])
	}
}

func TestOpenAPIMultipart(t *testing.T) {
	rs, err := openAPI([]byte(`openapi: 3.0.0
paths:
  /imports:
    post:
      requestBody:
        content:
          multipart/form-data:
            schema:
              type: object
              required: [kind, file, attachments]
              properties:
                kind: {type: string, enum: [orders]}
                file: {type: string, format: binary}
                attachments: {type: array, items: {type: string, format: binary}}
            encoding:
              file: {contentType: "text/csv, application/vnd.ms-excel"}
  /avatar:
    put:
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {url: {type: string}}}
          multipart/form-data:
            schema: {type: object, properties: {avatar: {type: string, format: binary}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	imp := rs[1]
	if imp.Path != "/imports" || imp.BodyType != routes.Multipart || string(imp.Body) != `{"kind":"orders"}` ||
		!reflect.DeepEqual(imp.Files, []routes.FileField{{Field: "attachments", Kind: "txt"}, {Field: "file", Kind: "csv"}}) {
		t.Fatalf("imports: %+v %s", imp, imp.Body)
	}
	if av := rs[0]; av.BodyType != "" || av.Files != nil {
		t.Fatalf("a JSON body is preferred: %+v", av)
	}

	rs, err = openAPI([]byte(`swagger: "2.0"
paths:
  /upload:
    post:
      consumes: [multipart/form-data]
      parameters:
        - {in: formData, name: logo, type: file}
        - {in: formData, name: title, type: string, required: true}
        - {in: formData, name: note, type: string}
`))
	if err != nil {
		t.Fatal(err)
	}
	if up := rs[0]; string(up.Body) != `{"title":"driftgate"}` || !reflect.DeepEqual(up.Files, []routes.FileField{{Field: "logo", Kind: "png"}}) {
		t.Fatalf("swagger 2: %+v %s", up, up.Body)
	}
}
