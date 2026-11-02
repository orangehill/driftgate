package scan

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/sheet"
	"github.com/orangehill/driftgate/internal/upload"
)

// TestPublic: a public body is scanned as the repository would hold
// it: decoded from gzip (with or without a Content-Encoding), from UTF-16
// and UTF-32, a workbook part by part; what cannot be read (another
// encoding, an image, a PDF, text with NUL bytes or in a charset that
// changes ASCII) is not taken for clean. Text and JSON are scanned as they
// always were.
func TestPublic(t *testing.T) {
	d, _ := pii.New(nil)
	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		w.Write([]byte(s))
		w.Close()
		return b.Bytes()
	}
	png, _ := upload.Synthetic(upload.PNG)
	pdf, _ := upload.Synthetic(upload.PDF)
	for _, c := range []struct {
		name, ct, enc string
		body          []byte
		personal      bool
		unscanned     bool
	}{
		{"json", "application/json", "", []byte(`{"email":"marko@example.rs"}`), true, false},
		{"clean json", "application/json", "", []byte(`{"id":1}`), false, false},
		{"gzip", "application/json", "gzip", gz(`{"email":"marko@example.rs"}`), true, false},
		{"clean gzip", "application/json", "gzip", gz(`{"id":1}`), false, false},
		{"br", "application/json", "br", []byte{0x1b, 0x03}, false, true},
		{"broken gzip", "application/json", "gzip", []byte("not gzip"), false, true},
		{"latin-1 html", "text/html; charset=windows-1250", "", []byte("<p>\xe8 marko@example.rs</p>"), true, false},
		{"text without a type", "", "", []byte("call +381 64 123 4567"), true, false},
		{"png", "image/png", "", png, false, true},
		{"pdf", "application/pdf", "", pdf, false, true},
		{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "", sheet.WriteXLSX([][]string{{"name", "email"}, {"Marko", "marko@example.rs"}}), true, false},
		{"clean xlsx", "application/octet-stream", "", sheet.WriteXLSX([][]string{{"name", "value"}, {"x", "1"}}), false, false},
		{"token", "application/json", "", []byte(`{"access_token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlLXNpZ25hdHVyZQ"}`), true, false},
		// Text whose ASCII has NUL bytes, decoded first.
		{"utf-16 with a bom", "text/plain; charset=utf-16", "", utf16le("\ufeffwrite marko@example.rs"), true, false},
		{"utf-16 with a bom, no type", "", "", utf16le("\ufeffmarko@example.rs"), true, false},
		{"utf-16le without a bom", "text/plain; charset=UTF-16LE", "", utf16le("marko@example.rs"), true, false},
		{"utf-16 big endian", "text/csv; charset=utf-16", "", utf16be("ime,email\nMarko,marko@example.rs"), true, false},
		{"utf-16 json", "application/json", "", utf16le("\ufeff{\"email\":\"marko@example.rs\"}"), true, false},
		{"utf-32", "text/plain; charset=utf-32", "", []byte("\x00\x00\xfe\xff\x00\x00\x00m\x00\x00\x00@\x00\x00\x00x"), false, false},
		{"clean utf-16", "text/plain; charset=utf-16", "", utf16le("\ufeffno data here"), false, false},
		{"broken utf-16", "text/plain; charset=utf-16", "", []byte("\xff\xfea\x00\x00\xd8"), false, true},
		{"utf-16 without a bom or charset", "", "", utf16le("marko@example.rs"), false, true},
		{"nul in a typed text", "text/plain", "", []byte("marko\x00@example.rs"), false, true},
		{"utf-7", "text/plain; charset=utf-7", "", []byte("marko+AEA-example.rs"), false, true},
		{"gzip without content-encoding", "application/json", "", gz(`{"email":"marko@example.rs"}`), true, false},
		{"gzip without a type", "", "", gz(`marko@example.rs`), true, false},
		{"gzip twice", "application/json", "gzip", gz(string(gz(`{"email":"marko@example.rs"}`))), true, false},
		{"compressed, not gzip", "application/json", "", []byte("\x28\xb5\x2f\xfd\x04\x00\x01\x02marko"), false, true},
	} {
		headers := [][2]string{{"content-encoding", c.enc}, {"content-type", c.ct}}
		if c.enc == "" {
			headers = headers[1:]
		}
		findings, unscanned := Public(d, headers, c.body)
		if (len(findings) > 0) != c.personal || (unscanned != "") != c.unscanned {
			t.Errorf("%s: findings %+v, unscanned %q", c.name, findings, unscanned)
		}
	}
}

func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

func utf16be(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}
