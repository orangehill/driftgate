package generate

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/routes"
)

func TestGenerateMultipart(t *testing.T) {
	p := project(t, config.Config{Templates: []string{"ok", "empty-body"}})
	inv := &routes.Inventory{Routes: []routes.Route{{Methods: []string{"POST"}, Path: "/api/imports",
		Body: json.RawMessage(`{"kind":"orders","meta":{"year":2026,"final":true},"tags":["a",null]}`), BodyType: routes.Multipart,
		Files: []routes.FileField{{Field: "file", Kind: "xlsx"}}}}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	reqs := res.Catalog.Requests
	if len(reqs) != 2 {
		t.Fatalf("%+v", reqs)
	}
	ok := reqs[0]
	want := &catalog.Multipart{
		Fields: [][2]string{{"kind", "orders"}, {"meta[year]", "2026"}, {"meta[final]", "1"}, {"tags[0]", "a"}, {"tags[1]", ""}},
		Files:  []catalog.File{{Field: "file", Synthetic: "xlsx"}},
	}
	if ok.Body != nil || ok.Headers != nil || !reflect.DeepEqual(ok.Multipart, want) {
		t.Fatalf("ok: %+v %+v", ok, ok.Multipart)
	}
	if empty := reqs[1]; empty.Body != nil || !reflect.DeepEqual(empty.Multipart, &catalog.Multipart{}) {
		t.Fatalf("empty-body: %+v", empty)
	}
	if err := res.Catalog.Validate(); err != nil {
		t.Fatal(err)
	}
}
