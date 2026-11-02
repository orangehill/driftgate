package files

import (
	"bytes"
	"context"
	"errors"
	"image/gif"
	"image/png"
	"testing"
)

// The fuzz targets run as tests on their seeds; "go test -fuzz FuzzPDF"
// (one target at a time) explores further. A reader must not panic (the
// recover in Placeholder would hide it as an unreadable file), and the
// placeholder must be the same on every run.

func fuzzPlaceholder(t *testing.T, data []byte, name string) {
	if len(data) > 64<<10 {
		return
	}
	opt := Options{MaxPixels: 1 << 16}
	var out, again bytes.Buffer
	info, err := Placeholder(context.Background(), bytes.NewReader(data), int64(len(data)), name, &out, opt)
	if errors.Is(err, errUnreadable) {
		t.Fatalf("%s: %v", name, err)
	}
	if err != nil {
		return
	}
	if _, err := Placeholder(context.Background(), bytes.NewReader(data), int64(len(data)), name, &again, opt); err != nil || !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Fatalf("%s: output differs between runs (%v)", name, err)
	}
	switch info.Kind {
	case PNG:
		if _, err := png.Decode(&out); err != nil {
			t.Fatalf("placeholder %+v: %v", info, err)
		}
	case GIF:
		if _, err := gif.Decode(&out); err != nil {
			t.Fatalf("placeholder %+v: %v", info, err)
		}
	case PDF:
		if pages, err := pdfPages(out.Bytes()); err != nil || len(pages) != info.Pages {
			t.Fatalf("placeholder %+v: %d pages, %v", info, len(pages), err)
		}
	}
}

func FuzzPDF(f *testing.F) {
	f.Add(samplePDF(false))
	f.Add(samplePDF(true))
	f.Add([]byte("%PDF-1.4\n1 0 obj\n<< /Length 10000000000 >>\nstream\nxx\nendstream\nendobj\n"))
	f.Add([]byte("%PDF-1.4\n1 0 obj (\n2 0 obj <\n3 0 obj<<>>stream\n4 0 obj [ %\ntrailer << /Root 1 0 R >>"))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzPlaceholder(t, data, "a.pdf") })
}

func FuzzImage(f *testing.F) {
	var src bytes.Buffer
	png.Encode(&src, pattern(4, 3))
	f.Add(src.Bytes())
	f.Add([]byte("\xff\xd8\xff\xe0\x00\x04ab\xff\xc0\x00\x11\x08\x00\x10\x00\x20"))
	f.Add([]byte("GIF89a\x10\x00\x20\x00\x00\x00\x00"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\x0f\x00\x00\x0f\x00\x00"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8L\x05\x00\x00\x00\x2f\x07\xc0\x01\x00"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x0a\x00\x00\x00\x00\x00\x00\x9d\x01\x2a\x10\x00\x20\x00"))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzPlaceholder(t, data, "a.img") })
}

func FuzzSVG(f *testing.F) {
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="5px" viewBox="0 0 10 5"/>`))
	f.Add([]byte(`<?xml version="1.0"?><!DOCTYPE svg><svg width="1e400" viewBox="0,0,1,nan"><title>x</title></svg>`))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzPlaceholder(t, data, "a.svg") })
}

func FuzzZip(f *testing.F) {
	f.Add(makeZip("[Content_Types].xml", "<Types/>", "word/document.xml", "<w:t/>"))
	f.Add(makeZip("mimetype", "application/vnd.oasis.opendocument.text", "content.xml", "x"))
	f.Add([]byte("PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzPlaceholder(t, data, "a.zip") })
}

func FuzzText(f *testing.F) {
	f.Add([]byte("a,b;c\n1,2\n"), "a.csv")
	f.Add([]byte(`{"a": [1, 2]}`), "a.json")
	f.Add([]byte("<?xml version=\"1.0\"?><a/>"), "a.xml")
	f.Add([]byte("<!doctype html><p>x"), "a.html")
	f.Add([]byte("plain\ttext\n"), "a.txt")
	f.Fuzz(func(t *testing.T, data []byte, name string) { fuzzPlaceholder(t, data, name) })
}
