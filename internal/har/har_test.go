package har

import (
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/routes"
)

const sample = `{"log":{"entries":[
 {"request":{"method":"GET","url":"https://app.test/api/news?page=2","headers":[{"name":"Accept","value":"application/json"},{"name":"Cookie","value":"session=secret"}]}},
 {"request":{"method":"GET","url":"https://app.test/api/news?page=2","headers":[]}},
 {"request":{"method":"POST","url":"https://app.test/api/faq","headers":[{"name":"Authorization","value":"Bearer secret"},{"name":"Content-Type","value":"application/json"}],"postData":{"mimeType":"application/json","text":"{ \"q\": \"?\" }"}}},
 {"request":{"method":"POST","url":"https://app.test/login","headers":[],"postData":{"mimeType":"application/x-www-form-urlencoded","text":"a=b"}}},
 {"request":{"method":"GET","url":"https://app.test/build/app.js","headers":[]}},
 {"request":{"method":"GET","url":"https://cdn.other.test/api/x","headers":[]}}
]}}`

func TestRead(t *testing.T) {
	cands, notes, err := Read([]byte(sample), "https://app.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 { // the repeated GET counts once
		t.Fatalf("candidates %+v", cands)
	}
	if c := cands[0]; c.Path != "/api/news?page=2" || !c.Auth || len(c.Headers) != 1 || c.Headers[0][0] != "accept" {
		t.Errorf("first: %+v", c)
	}
	if c := cands[1]; string(c.Body) != `{"q":"?"}` || !c.Auth {
		t.Errorf("post: %+v", c)
	}
	for _, c := range cands {
		for _, h := range c.Headers {
			if strings.Contains(h[1], "secret") {
				t.Fatalf("secret header kept: %+v", c)
			}
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0].Reason, "not JSON") {
		t.Errorf("notes %+v", notes)
	}
}

func TestReadLeavesSecretsOut(t *testing.T) {
	const f = `{"log":{"entries":[
 {"request":{"method":"GET","url":"https://h/api/x?access_token=SECRET123","headers":[]}},
 {"request":{"method":"POST","url":"https://h/api/login","headers":[],"postData":{"mimeType":"application/json","text":"{\"email\":\"a@b.c\",\"user\":{\"password\":\"hunter2\"}}"}}},
 {"request":{"method":"GET","url":"https://h/api/users?email=pera.peric@example.com","headers":[]}},
 {"request":{"method":"GET","url":"https://h/apiv2/users","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/news?page=2","headers":[]}}
]}}`
	cands, notes, err := Read([]byte(f), "https://h/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Path != "/news?page=2" {
		t.Fatalf("candidates %+v", cands)
	}
	if len(notes) != 3 {
		t.Fatalf("notes %+v", notes)
	}
	for _, n := range notes {
		if strings.Contains(n.Reason, "SECRET") || strings.Contains(n.Reason, "hunter2") {
			t.Fatalf("a note shows a secret: %+v", n)
		}
	}
}

// TestNotesHidePersonalData: a note names the request without its query,
// and with the personal data or token in its path masked.
func TestNotesHidePersonalData(t *testing.T) {
	const f = `{"log":{"entries":[
 {"request":{"method":"GET","url":"https://h/api/users/pera.peric@example.com/orders","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/users/pera.peric%40example.com","headers":[]}},
 {"request":{"method":"POST","url":"https://h/api/notes?email=pera.peric@example.com","headers":[],"postData":{"mimeType":"text/plain","text":"hi"}}},
 {"request":{"method":"POST","url":"https://h/api/notes?email=pera.peric@example.com","headers":[],"postData":{"mimeType":"application/json","text":"{"}}},
 {"request":{"method":"GET","url":"https://h/api/users?email=pera.peric@example.com","headers":[]}}
]}}`
	_, notes, err := Read([]byte(f), "https://h/api")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /users/{personal}/orders", "GET /users/{personal}", "POST /notes", "POST /notes", "GET /users"}
	if len(notes) != len(want) {
		t.Fatalf("notes %+v", notes)
	}
	for i, n := range notes {
		if n.Request != want[i] || strings.Contains(n.Request+n.Reason, "pera") {
			t.Errorf("note %d: %+v, want %s", i, n, want[i])
		}
	}
	if got := MaskPath("/reset/eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U/pera.peric@example.com"); got != "/reset/{token}/{personal}" {
		t.Errorf("MaskPath: %s", got)
	}
}

// TestReadPersonalLaterOrUnscanned: personal data in a later response of a
// request already seen marks it Personal; so does a response body the file
// does not hold whole.
func TestReadPersonalLaterOrUnscanned(t *testing.T) {
	const f = `{"log":{"entries":[
 {"request":{"method":"GET","url":"https://h/api/feed?since=1","headers":[]},"response":{"content":{"size":12,"mimeType":"application/json","text":"{\"items\":[]}"}}},
 {"request":{"method":"GET","url":"https://h/api/feed?since=2","headers":[]},"response":{"content":{"size":40,"mimeType":"application/json","text":"{\"items\":[{\"from\":\"pera.peric@example.com\"}]}"}}},
 {"request":{"method":"GET","url":"https://h/api/news","headers":[]},"response":{"content":{"size":12,"mimeType":"application/json"}}},
 {"request":{"method":"GET","url":"https://h/api/tags","headers":[]},"response":{"content":{"size":12,"mimeType":"application/json","text":"%%%","encoding":"base64"}}},
 {"request":{"method":"GET","url":"https://h/api/clean","headers":[]},"response":{"content":{"size":2,"mimeType":"application/json","text":"{}"}}},
 {"request":{"method":"GET","url":"https://h/api/empty","headers":[]},"response":{"content":{"size":0,"mimeType":"application/json"}}}
]}}`
	cands, _, err := ReadWith([]byte(f), "https://h/api", Options{Routes: &routes.Inventory{}, Personal: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Path] = c.Personal
	}
	want := map[string]bool{"/feed?since=1": true, "/news": true, "/tags": true, "/clean": false, "/empty": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("personal %v, want %v", got, want)
	}
}
