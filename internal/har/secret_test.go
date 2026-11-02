package har

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// TestReadLeavesCredentialsOut: the credentials that passed the name
// filter when it needed _ or - around a word, and values shaped like a
// credential under an innocent name.
func TestReadLeavesCredentialsOut(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlLXNpZ25hdHVyZQ"
	const hexToken = "9f86d081884c7d659a2feaa0c55ad015"
	const random = "xK3pQ9vZ2mT7wR4yL8nB1cF6"
	f := `{"log":{"entries":[
 {"request":{"method":"POST","url":"https://h/api/password","headers":[],"postData":{"mimeType":"application/json","text":"{\"currentPassword\":\"old-one\",\"newPassword\":\"new-one\"}"}}},
 {"request":{"method":"POST","url":"https://h/api/session","headers":[],"postData":{"mimeType":"application/json","text":"{\"user\":{\"authToken\":\"abc\"}}"}}},
 {"request":{"method":"GET","url":"https://h/api/me?jwt=` + jwt + `","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/sso?ticket=ST-12-xK3pQ9vZ2mT7wR4y-cas","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/reset-password/` + hexToken + `","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/me?t=` + jwt + `","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/download?x=` + random + `","headers":[]}},
 {"request":{"method":"POST","url":"https://h/api/verify","headers":[],"postData":{"mimeType":"application/json","text":"{\"value\":\"Bearer ` + jwt + `\"}"}}},
 {"request":{"method":"GET","url":"https://h/api/invite/` + random + `","headers":[]}},
 {"request":{"method":"POST","url":"https://h/api/form","headers":[],"postData":{"mimeType":"multipart/form-data; boundary=x","params":[{"name":"x","value":"` + random + `"}]}}},
 {"request":{"method":"GET","url":"https://h/api/orders?verification_code=123456","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/csrf?XSRF-TOKEN=a","headers":[]}}
]}}`
	cands, notes, err := Read([]byte(f), "https://h/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 || len(notes) != 12 {
		t.Fatalf("candidates %+v notes %+v", cands, notes)
	}
	for _, n := range notes {
		for _, s := range []string{"old-one", "new-one", jwt[:20], hexToken, random, "ST-12"} {
			if strings.Contains(n.Request+n.Reason, s) {
				t.Fatalf("a note shows a secret: %+v", n)
			}
		}
	}
	if notes[4].Request != "GET /reset-password/{token}" {
		t.Errorf("path token note: %+v", notes[4])
	}
}

// TestReadKeepsIdentifiers: identifiers, slugs and codes that are not
// credentials stay in the catalog.
func TestReadKeepsIdentifiers(t *testing.T) {
	f := `{"log":{"entries":[
 {"request":{"method":"GET","url":"https://h/api/items/550e8400-e29b-41d4-a716-446655440000","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/users/507f1f77bcf86cd799439011","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/posts/my-first-post-2024-10-30-hello","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/rates?country_code=RS&currency_code=RSD&author=1&state=open","headers":[]}},
 {"request":{"method":"GET","url":"https://h/api/x?view=getUserProfileById2024&n=12345678901234567890123","headers":[]}},
 {"request":{"method":"POST","url":"https://h/api/a","headers":[],"postData":{"mimeType":"application/json","text":"{\"status_code\":200,\"title\":\"A long title, written by hand in 2024\"}"}}}
]}}`
	cands, notes, err := Read([]byte(f), "https://h/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 6 || len(notes) != 0 {
		t.Fatalf("candidates %+v notes %+v", cands, notes)
	}
}

func TestSecretName(t *testing.T) {
	for _, n := range []string{"password", "currentPassword", "new_password", "authToken", "access_token", "accesstoken",
		"XSRFToken", "X-XSRF-TOKEN", "csrfmiddlewaretoken", "apiKey", "api_key", "jwt", "ticket", "nonce", "code",
		"resetCode", "otp", "pin", "session_id", "refresh", "access", "clientSecret", "hash", "signature"} {
		if !secretName.MatchString(n) {
			t.Errorf("%s is not taken for a credential", n)
		}
	}
	for _, n := range []string{"page", "author", "country_code", "zipCode", "status_code", "state", "accessibility",
		"keyword", "sort", "passport", "pinned", "secretary", "title"} {
		if secretName.MatchString(n) {
			t.Errorf("%s is taken for a credential", n)
		}
	}
}

// TestRandomTokensFound: generated tokens are found in about 98% of 32
// characters of base64url (randomToken).
func TestRandomTokensFound(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	rng := rand.New(rand.NewPCG(1, 2))
	missed := 0
	for range 1000 {
		b := make([]byte, 32)
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		if !secretValue(string(b)) {
			missed++
		}
	}
	if missed > 30 {
		t.Fatalf("%d of 1000 random tokens missed", missed)
	}
}
