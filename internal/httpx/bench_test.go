package httpx

import (
	"bytes"
	"net/http"
	"testing"
)

func benchRead(b *testing.B, n int, known, offHeap bool) {
	body := bytes.Repeat([]byte(`{"id":1,"name":"x"},`), n/20)
	length := int64(-1)
	if known {
		length = int64(len(body))
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, reg, err := readBody(bytes.NewReader(body), length, offHeap)
		if err != nil {
			b.Fatal(err)
		}
		reg.free()
	}
}

func BenchmarkReadBodyKnownSmall(b *testing.B)   { benchRead(b, 8<<10, true, true) }
func BenchmarkReadBodyKnownLarge(b *testing.B)   { benchRead(b, 8<<20, true, true) }
func BenchmarkReadBodyChunkedSmall(b *testing.B) { benchRead(b, 8<<10, false, true) }
func BenchmarkReadBodyChunkedLarge(b *testing.B) { benchRead(b, 8<<20, false, true) }

// BenchmarkHeaders masks the headers of a typical API response.
func BenchmarkHeaders(b *testing.B) {
	h := http.Header{}
	for _, kv := range [][2]string{
		{"Content-Type", "application/json"}, {"Content-Length", "1234"}, {"Date", "Mon, 15 Jan 2024 10:30:00 GMT"},
		{"Server", "nginx"}, {"Cache-Control", "no-cache, private"}, {"Etag", `W/"5d41402abc4b2a76b9719d911017c592"`},
		{"Vary", "Accept-Encoding"}, {"X-Request-Id", "Xa8sd7F6a8sDf7as8d9Fq2w3"}, {"X-Ratelimit-Remaining", "59"},
		{"Access-Control-Allow-Origin", "*"}, {"Strict-Transport-Security", "max-age=31536000"},
		{"Set-Cookie", "laravel_session=abc123; path=/; httponly"}, {"Location", "https://app.test/items?page=2#top"},
	} {
		h.Add(kv[0], kv[1])
	}
	b.ReportAllocs()
	for b.Loop() {
		Headers(h)
	}
}
