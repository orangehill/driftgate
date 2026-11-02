package upload

import (
	"bytes"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/sheet"
)

func TestSynthetic(t *testing.T) {
	for _, k := range Kinds() {
		a, err := Synthetic(k)
		if err != nil {
			t.Fatal(k, err)
		}
		b, _ := Synthetic(k)
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Fatalf("%s: empty or not deterministic", k)
		}
		var derr error
		switch k {
		case PNG:
			_, derr = png.Decode(bytes.NewReader(a))
		case JPEG:
			_, derr = jpeg.Decode(bytes.NewReader(a))
		case GIF:
			_, derr = gif.Decode(bytes.NewReader(a))
		case XLSX:
			_, derr = sheet.ReadXLSX(a)
		case PDF:
			if !bytes.HasPrefix(a, []byte("%PDF-")) || !bytes.HasSuffix(a, []byte("%%EOF\n")) {
				t.Fatalf("pdf: %q", a)
			}
		}
		if derr != nil {
			t.Fatalf("%s: %v", k, derr)
		}
	}
	if _, err := Synthetic("exe"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestKindOf(t *testing.T) {
	for _, c := range [][3]string{
		{"report.XLSX", "", XLSX}, {"xls", "", XLSX}, {"", "text/csv; charset=utf-8", CSV}, {"a.bin", "image/webp", PNG},
		{"photo.jpeg", "", JPEG}, {"", "application/pdf", PDF}, {"a.exe", "application/octet-stream", ""}, {"notes", "text/markdown", TXT},
	} {
		if got := KindOf(c[0], c[1]); got != c[2] {
			t.Errorf("KindOf(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestEncode(t *testing.T) {
	fields := [][2]string{{"kind", "orders"}, {"tags[]", "a"}, {"tags[]", "b"}}
	files := []Part{{Field: "file", Name: `x"y.csv`, Type: "text/csv", Data: []byte("a,b\n1,2\n")}}
	body, ct, err := Encode(fields, files)
	if err != nil {
		t.Fatal(err)
	}
	again, ct2, _ := Encode(fields, files)
	if !bytes.Equal(body, again) || ct != ct2 {
		t.Fatal("the same request gave another body")
	}
	other, ct3, _ := Encode(fields, []Part{{Field: "file", Name: "x.csv", Data: []byte("other")}})
	if ct3 == ct || bytes.Equal(other, body) {
		t.Fatal("another body has the same boundary")
	}
	mt, ps, _ := mime.ParseMediaType(ct)
	if mt != "multipart/form-data" || !strings.HasPrefix(ps["boundary"], "driftgate-") {
		t.Fatal(ct)
	}
	r := multipart.NewReader(bytes.NewReader(body), ps["boundary"])
	var got []string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		got = append(got, p.FormName()+"|"+p.FileName()+"|"+p.Header.Get("Content-Type")+"|"+string(data))
	}
	want := []string{"kind|||orders", "tags[]|||a", "tags[]|||b", `file|x"y.csv|text/csv|a,b` + "\n1,2\n"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("parts:\n%s", strings.Join(got, "\n"))
	}
}

// A part read through Open gives the same bytes as one in memory, in a
// buffer made once at its size; content whose length changed between the
// two reads is an error.
func TestEncodeOpen(t *testing.T) {
	data := bytes.Repeat([]byte("a,b\n1,2\n"), 100<<10)
	fields := [][2]string{{"kind", "orders"}}
	want, wantCT, err := Encode(fields, []Part{{Field: "file", Name: "x.csv", Type: "text/csv", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	opens := 0
	open := func() (io.ReadCloser, error) {
		opens++
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	got, ct, err := Encode(fields, []Part{{Field: "file", Name: "x.csv", Type: "text/csv", Open: open, Size: int64(len(data))}})
	if err != nil || ct != wantCT || !bytes.Equal(got, want) {
		t.Fatalf("through Open: %v, %s", err, ct)
	}
	if opens != 2 || cap(got) > len(got)+1024 {
		t.Fatalf("%d opens, %d bytes in a buffer of %d", opens, len(got), cap(got))
	}
	grown := func() (io.ReadCloser, error) {
		opens++
		return io.NopCloser(bytes.NewReader(data[:len(data)-opens%2])), nil
	}
	if _, _, err := Encode(fields, []Part{{Field: "file", Name: "x.csv", Open: grown, Size: int64(len(data))}}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("content that changed: %v", err)
	}
}
