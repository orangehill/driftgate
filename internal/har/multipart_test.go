package har

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
)

func TestReadMultipart(t *testing.T) {
	body := "--b1\r\nContent-Disposition: form-data; name=\"kind\"\r\n\r\norders\r\n" +
		"--b1\r\nContent-Disposition: form-data; name=\"file\"; filename=\"Petar Petrovic 2024.xlsx\"\r\nContent-Type: application/vnd.openxmlformats-officedocument.spreadsheetml.sheet\r\n\r\nPK\x03\x04 SECRET-ROWS\r\n--b1--\r\n"
	text, _ := json.Marshal(body)
	f := `{"log":{"entries":[
 {"request":{"method":"POST","url":"https://h/api/imports","headers":[{"name":"Content-Type","value":"multipart/form-data; boundary=b1"},{"name":"Accept","value":"application/json"}],
  "postData":{"mimeType":"multipart/form-data; boundary=b1","text":` + string(text) + `}}},
 {"request":{"method":"POST","url":"https://h/api/avatar","headers":[],
  "postData":{"mimeType":"multipart/form-data; boundary=x","params":[{"name":"photo","fileName":"me.jpg","contentType":"image/jpeg"},{"name":"caption","value":"hi"}]}}},
 {"request":{"method":"POST","url":"https://h/api/profile","headers":[],
  "postData":{"mimeType":"multipart/form-data; boundary=x","params":[{"name":"contact","value":"pera.peric@example.com"}]}}},
 {"request":{"method":"POST","url":"https://h/api/login","headers":[],
  "postData":{"mimeType":"multipart/form-data; boundary=x","params":[{"name":"password","value":"hunter2"}]}}}
]}}`
	cands, notes, err := Read([]byte(f), "https://h/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || len(notes) != 2 {
		t.Fatalf("candidates %+v notes %+v", cands, notes)
	}
	want := &catalog.Multipart{Fields: [][2]string{{"kind", "orders"}}, Files: []catalog.File{{Field: "file", Synthetic: "xlsx"}}}
	if c := cands[0]; !reflect.DeepEqual(c.Multipart, want) || c.Body != nil || !reflect.DeepEqual(c.Headers, [][2]string{{"accept", "application/json"}}) {
		t.Fatalf("imports: %+v %+v", c, c.Multipart)
	}
	want = &catalog.Multipart{Fields: [][2]string{{"caption", "hi"}}, Files: []catalog.File{{Field: "photo", Synthetic: "jpeg"}}}
	if c := cands[1]; !reflect.DeepEqual(c.Multipart, want) {
		t.Fatalf("avatar: %+v", c.Multipart)
	}
	all, _ := json.Marshal(cands)
	if strings.Contains(string(all), "SECRET") || strings.Contains(string(all), "Petrovic") {
		t.Fatalf("uploaded content or file name kept: %s", all)
	}
}
