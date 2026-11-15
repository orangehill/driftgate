package files

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// marker is planted in every sample; it must never reach a placeholder.
const marker = "Jovana Petrovic jovana.petrovic@example.com 0101990710006"

func place(t *testing.T, name string, data []byte) (Info, []byte) {
	t.Helper()
	var out bytes.Buffer
	info, err := Placeholder(context.Background(), bytes.NewReader(data), int64(len(data)), name, &out, Options{})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	// Deterministic: the same input gives the same bytes.
	var again bytes.Buffer
	if _, err := Placeholder(context.Background(), bytes.NewReader(data), int64(len(data)), name, &again, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Fatalf("%s: output differs between runs", name)
	}
	for _, s := range []string{marker, "jovana", "Jovana", "0101990710006"} {
		if bytes.Contains(out.Bytes(), []byte(s)) {
			t.Fatalf("%s: marker %q in the placeholder", name, s)
		}
	}
	return info, out.Bytes()
}

// pattern is a distinctive image: a checkerboard of saturated colours.
func pattern(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x/3+y/3)%2 == 0 {
				img.Set(x, y, color.RGBA{255, 0, 40, 255})
			} else {
				img.Set(x, y, color.RGBA{10, 200, 255, 255})
			}
		}
	}
	return img
}

// assertFlat decodes an image and checks it is uniformly grey.
func assertFlat(t *testing.T, img image.Image, w, h int) {
	t.Helper()
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("decoded %v, want %d×%d", b, w, h)
	}
	for y := 0; y < h; y += max(1, h/17) {
		for x := 0; x < w; x += max(1, w/17) {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 != grey || g>>8 != grey || b>>8 != grey {
				t.Fatalf("pixel %d,%d = %d,%d,%d, want flat grey", x, y, r>>8, g>>8, b>>8)
			}
		}
	}
}

var sizes = [][2]int{{1, 1}, {7, 9}, {64, 48}, {333, 101}, {1920, 1080}}

func TestPNG(t *testing.T) {
	for _, s := range sizes {
		var src bytes.Buffer
		png.Encode(&src, pattern(s[0], s[1]))
		// A text chunk with the marker, before IEND.
		raw := src.Bytes()
		chunk := pngChunk("tEXt", []byte("Author\x00"+marker))
		raw = append(append(bytes.Clone(raw[:len(raw)-12]), chunk...), raw[len(raw)-12:]...)
		info, out := place(t, "photo.png", raw)
		if info.Kind != PNG || info.MediaType != "image/png" || info.Width != s[0] || info.Height != s[1] || info.Fallback != "" {
			t.Fatalf("info %+v", info)
		}
		img, err := png.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		assertFlat(t, img, s[0], s[1])
	}
}

func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crcOf(typ, data))
	return b.Bytes()
}

func crcOf(typ string, data []byte) uint32 {
	var b bytes.Buffer
	b.WriteString(typ)
	b.Write(data)
	return crc32IEEE(b.Bytes())
}

func TestJPEGWithEXIF(t *testing.T) {
	for _, s := range sizes {
		var src bytes.Buffer
		jpeg.Encode(&src, pattern(s[0], s[1]), &jpeg.Options{Quality: 95})
		// APP1 Exif segment (with the marker as the artist) and a comment,
		// right after SOI.
		exif := append([]byte("Exif\x00\x00MM\x00\x2A\x00\x00\x00\x08"), marker...)
		seg := func(m byte, p []byte) []byte {
			return append([]byte{0xFF, m, byte((len(p) + 2) >> 8), byte(len(p) + 2)}, p...)
		}
		raw := append([]byte{0xFF, 0xD8}, seg(0xE1, exif)...)
		raw = append(raw, seg(0xFE, []byte(marker))...)
		raw = append(raw, src.Bytes()[2:]...)
		if _, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		info, out := place(t, "photo.jpg", raw)
		if info.Kind != JPEG || info.Width != s[0] || info.Height != s[1] || info.Fallback != "" {
			t.Fatalf("info %+v", info)
		}
		if bytes.Contains(out, []byte("Exif")) {
			t.Fatal("EXIF in the placeholder")
		}
		img, err := jpeg.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		assertFlat(t, img, s[0], s[1])
	}
}

func TestGIF(t *testing.T) {
	for _, s := range append(sizes, [2]int{300, 300}) { // 90000 pixels: the LZW table resets
		var src bytes.Buffer
		p := image.NewPaletted(image.Rect(0, 0, s[0], s[1]), color.Palette{color.RGBA{255, 0, 40, 255}, color.RGBA{10, 200, 255, 255}})
		for i := range p.Pix {
			p.Pix[i] = uint8(i % 2)
		}
		gif.Encode(&src, p, nil)
		info, out := place(t, "anim.gif", src.Bytes())
		if info.Kind != GIF || info.Width != s[0] || info.Height != s[1] {
			t.Fatalf("info %+v", info)
		}
		img, err := gif.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		assertFlat(t, img, s[0], s[1])
	}
}

func TestWebP(t *testing.T) {
	riff := func(fourcc string, data []byte) []byte {
		var b bytes.Buffer
		b.WriteString("RIFF")
		binary.Write(&b, binary.LittleEndian, uint32(4+8+len(data)))
		b.WriteString("WEBP" + fourcc)
		binary.Write(&b, binary.LittleEndian, uint32(len(data)))
		b.Write(data)
		return b.Bytes()
	}
	lossy := func(w, h int) []byte {
		d := []byte{0x50, 0x01, 0x00, 0x9D, 0x01, 0x2A, 0, 0, 0, 0}
		binary.LittleEndian.PutUint16(d[6:], uint16(w))
		binary.LittleEndian.PutUint16(d[8:], uint16(h))
		return riff("VP8 ", append(d, marker...))
	}
	extended := func(w, h int) []byte {
		d := []byte{0x10, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)}
		return riff("VP8X", append(d, marker...))
	}
	var lossless bytes.Buffer
	writeWebP(context.Background(), &lossless, 77, 33)
	cases := []struct {
		name string
		raw  []byte
		w, h int
	}{
		{"lossy", lossy(640, 480), 640, 480},
		{"extended", extended(5000, 3), 5000, 3},
		{"lossless", append(lossless.Bytes(), marker...), 77, 33},
	}
	if cwebp, err := exec.LookPath("cwebp"); err == nil {
		dir := t.TempDir()
		var src bytes.Buffer
		png.Encode(&src, pattern(123, 45))
		os.WriteFile(filepath.Join(dir, "in.png"), src.Bytes(), 0o600)
		for _, args := range [][]string{{"-q", "80"}, {"-lossless"}} {
			out := filepath.Join(dir, "out.webp")
			if err := exec.Command(cwebp, append(args, "-quiet", "-metadata", "all", filepath.Join(dir, "in.png"), "-o", out)...).Run(); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(out)
			cases = append(cases, struct {
				name string
				raw  []byte
				w, h int
			}{"cwebp " + args[0], raw, 123, 45})
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, out := place(t, "pic.webp", c.raw)
			if info.Kind != WebP || info.Width != c.w || info.Height != c.h || info.Fallback != "" {
				t.Fatalf("info %+v", info)
			}
			w, h, err := webpSize(bytes.NewReader(out))
			if err != nil || w != c.w || h != c.h {
				t.Fatalf("placeholder %d×%d %v", w, h, err)
			}
			dwebp, err := exec.LookPath("dwebp")
			if err != nil {
				return
			}
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "p.webp"), out, 0o600)
			if b, err := exec.Command(dwebp, filepath.Join(dir, "p.webp"), "-o", filepath.Join(dir, "p.png")).CombinedOutput(); err != nil {
				t.Fatalf("dwebp: %v\n%s", err, b)
			}
			f, _ := os.Open(filepath.Join(dir, "p.png"))
			defer f.Close()
			img, err := png.Decode(f)
			if err != nil {
				t.Fatal(err)
			}
			assertFlat(t, img, c.w, c.h)
		})
	}
}

func TestBadHeaderFallsBack(t *testing.T) {
	raw := append(bytes.Clone(pngMagic), []byte("\x00\x00\x00\x0dIHDR\x00\x00\x00\x00\x00\x00\x00\x00"+marker)...)
	info, out := place(t, "broken.png", raw)
	if info.Kind != PNG || info.Width != 1 || info.Height != 1 || info.Fallback == "" {
		t.Fatalf("info %+v", info)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
}

func TestPixelLimit(t *testing.T) {
	var src bytes.Buffer
	png.Encode(&src, pattern(100, 100))
	_, err := Placeholder(context.Background(), bytes.NewReader(src.Bytes()), int64(src.Len()), "a.png", &bytes.Buffer{}, Options{MaxPixels: 9999})
	if err == nil || !strings.Contains(err.Error(), ErrTooLarge.Error()) {
		t.Fatalf("err %v", err)
	}
	_, err = Placeholder(context.Background(), bytes.NewReader(src.Bytes()), int64(src.Len()), "a.png", &bytes.Buffer{}, Options{MaxSize: 10})
	if err == nil {
		t.Fatal("no size limit")
	}
}

func TestContentWinsOverExtension(t *testing.T) {
	var src bytes.Buffer
	png.Encode(&src, pattern(10, 20))
	info, out := place(t, "really-a-png.jpg", src.Bytes())
	if info.Kind != PNG || info.MediaType != "image/png" {
		t.Fatalf("info %+v", info)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
}

func TestSVG(t *testing.T) {
	cases := []struct {
		src, want string
		w, h      int
	}{
		{`<?xml version="1.0"?>
<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd">
<svg xmlns="http://www.w3.org/2000/svg" width="300" height="150px" viewBox="0,0 300 150"><title>` + marker + `</title><text>` + marker + `</text></svg>`,
			`<svg xmlns="http://www.w3.org/2000/svg" width="300" height="150px" viewBox="0 0 300 150"><rect x="0" y="0" width="300" height="150" fill="#808080"/></svg>` + "\n", 300, 150},
		{`<svg xmlns="http://www.w3.org/2000/svg" width="10cm" height="` + marker + `"><path d="M0 0"/></svg>`,
			`<svg xmlns="http://www.w3.org/2000/svg" width="10cm"><rect width="100%" height="100%" fill="#808080"/></svg>` + "\n", 0, 0},
	}
	for _, c := range cases {
		info, out := place(t, "logo.svg", []byte(c.src))
		if info.Kind != SVG || info.MediaType != "image/svg+xml" || info.Width != c.w || info.Height != c.h {
			t.Fatalf("info %+v", info)
		}
		if string(out) != c.want {
			t.Fatalf("got  %s\nwant %s", out, c.want)
		}
		if err := xml.Unmarshal(out, new(struct{})); err != nil {
			t.Fatal(err)
		}
	}
}

// samplePDF builds a PDF with text on every page, pages of different sizes
// (one inherited from the page tree, one rotated), optionally with the pages
// in a compressed object stream and a cross-reference stream.
func samplePDF(objStm bool) []byte {
	pages := []string{
		`<< /Type /Page /Parent 2 0 R /Contents 7 0 R >>`, // inherits A4
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 7 0 R >>`,
		`<< /Type /Page /Parent 6 0 R /MediaBox [0 0 1190.55 841.89] /CropBox [10 10 1180 830] /Rotate 90 /Contents 7 0 R >>`,
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	obj := func(n int, s string) { fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, s) }
	obj(1, `<< /Type /Catalog /Pages 2 0 R >>`)
	obj(2, `<< /Type /Pages /Kids [3 0 R 4 0 R 6 0 R] /Count 3 /MediaBox [0 0 595.28 841.89] >>`)
	obj(6, `<< /Type /Pages /Parent 2 0 R /Kids [5 0 R] /Count 1 >>`)
	content := "BT /F1 12 Tf 72 712 Td (" + marker + ") Tj ET"
	fmt.Fprintf(&b, "7 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	obj(8, `<< /Title (`+marker+`) /Author <4a6f76616e61> >>`)
	if !objStm {
		for i, p := range pages {
			obj(3+i, p)
		}
		b.WriteString("trailer\n<< /Root 1 0 R /Info 8 0 R >>\n%%EOF\n")
		return b.Bytes()
	}
	var head, body bytes.Buffer
	for i, p := range pages {
		fmt.Fprintf(&head, "%d %d ", 3+i, body.Len())
		body.WriteString(p + "\n")
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(head.Bytes())
	zw.Write(body.Bytes())
	zw.Close()
	fmt.Fprintf(&b, "9 0 obj\n<< /Type /ObjStm /N 3 /First %d /Filter /FlateDecode /Length %d >>\nstream\n", head.Len(), z.Len())
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	b.WriteString("10 0 obj\n<< /Type /XRef /Root 1 0 R /Size 11 /W [1 2 1] /Length 0 >>\nstream\n\nendstream\nendobj\n%%EOF\n")
	return b.Bytes()
}

func TestPDF(t *testing.T) {
	want := []pdfPage{
		{media: []float64{0, 0, 595.28, 841.89}},
		{media: []float64{0, 0, 612, 792}},
		{media: []float64{0, 0, 1190.55, 841.89}, crop: []float64{10, 10, 1180, 830}, rotate: 90},
	}
	for _, objStm := range []bool{false, true} {
		info, out := place(t, "contract.pdf", samplePDF(objStm))
		if info.Kind != PDF || info.MediaType != "application/pdf" || info.Pages != 3 || info.Fallback != "" {
			t.Fatalf("info %+v", info)
		}
		if bytes.Contains(out, []byte("BT")) || bytes.Contains(out, []byte("/Title")) || bytes.Contains(out, []byte("4a6f")) {
			t.Fatalf("text or metadata in the placeholder:\n%s", out)
		}
		got, err := pdfPages(out)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("pages %v, want %v", got, want)
		}
		// The cross-reference table points at every object.
		xref := bytes.LastIndex(out, []byte("\nxref\n")) + 1
		lines := strings.Split(string(out[xref:]), "\n")
		for i, l := range lines[3 : 3+3+len(want)] {
			var off int
			fmt.Sscanf(l, "%d", &off)
			if !bytes.HasPrefix(out[off:], []byte(fmt.Sprintf("%d 0 obj", i+1))) {
				t.Fatalf("xref entry %d points at %q", i+1, out[off:off+10])
			}
		}
	}
}

func TestPDFUnreadable(t *testing.T) {
	info, out := place(t, "x.pdf", []byte("%PDF-1.4\n"+marker+"\n%%EOF\n"))
	if info.Pages != 1 || info.Fallback == "" {
		t.Fatalf("info %+v", info)
	}
	if pages, _ := pdfPages(out); len(pages) != 1 {
		t.Fatalf("pages %v", pages)
	}
}

func makeZip(entries ...string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for i := 0; i < len(entries); i += 2 {
		w, _ := zw.Create(entries[i])
		w.Write([]byte(entries[i+1]))
	}
	zw.Close()
	return b.Bytes()
}

func TestOfficeAndZip(t *testing.T) {
	cases := []struct {
		name, kind string
		raw        []byte
		parts      []string
	}{
		{"cv.docx", DOCX, makeZip("[Content_Types].xml", "<Types/>", "word/document.xml", "<w:t>"+marker+"</w:t>", "docProps/core.xml", marker),
			[]string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"}},
		{"salaries.xlsx", XLSX, makeZip("[Content_Types].xml", "<Types/>", "xl/workbook.xml", "<workbook/>", "xl/sharedStrings.xml", marker),
			[]string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml"}},
		{"deck.pptx", PPTX, makeZip("[Content_Types].xml", "<Types/>", "ppt/presentation.xml", marker),
			[]string{"[Content_Types].xml", "_rels/.rels", "ppt/presentation.xml"}},
		{"letter.odt", ODT, makeZip("mimetype", "application/vnd.oasis.opendocument.text", "content.xml", marker),
			[]string{"mimetype", "META-INF/manifest.xml", "content.xml"}},
		{"table.ods", ODS, makeZip("mimetype", "application/vnd.oasis.opendocument.spreadsheet", "content.xml", marker), nil},
		{"jovana.zip", Zip, makeZip("jovana/"+marker+".txt", marker), []string{}},
	}
	for _, c := range cases {
		info, out := place(t, c.name, c.raw)
		if info.Kind != c.kind || info.MediaType != mediaTypes[c.kind] {
			t.Fatalf("%s: info %+v", c.name, info)
		}
		zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
			if f.Method != zip.Store || f.Flags&0x8 != 0 {
				t.Fatalf("%s: %s not stored plainly", c.name, f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if _, err := b.ReadFrom(rc); err != nil { // checks the CRC
				t.Fatal(err)
			}
			if strings.HasSuffix(f.Name, ".xml") || strings.HasSuffix(f.Name, ".rels") {
				if err := xml.Unmarshal(b.Bytes(), new(struct{})); err != nil {
					t.Fatalf("%s %s: %v", c.name, f.Name, err)
				}
			}
		}
		if c.parts != nil && strings.Join(names, ",") != strings.Join(c.parts, ",") {
			t.Fatalf("%s: entries %v", c.name, names)
		}
	}
}

func TestText(t *testing.T) {
	csvSrc := "ime;prezime;\"email; privatni\";jmbg\n" + strings.Repeat("Jovana;Petrovic;jovana.petrovic@example.com;0101990710006\n", 40)
	cases := []struct {
		name, src, kind string
		check           func([]byte) error
	}{
		{"users.json", `  [{"name":"` + marker + `"}]`, JSON, func(b []byte) error {
			var v []any
			return json.Unmarshal(b, &v)
		}},
		{"config", `{"owner":"` + marker + `"}`, JSON, func(b []byte) error {
			var v map[string]any
			return json.Unmarshal(b, &v)
		}},
		{"export.csv", csvSrc, CSV, func(b []byte) error {
			r := csv.NewReader(bytes.NewReader(b))
			r.Comma = ';'
			rows, err := r.ReadAll()
			if err != nil {
				return err
			}
			if len(rows) < 2 || len(rows[0]) != 4 || rows[0][0] != "column1" {
				return fmt.Errorf("rows %q", rows[:min(len(rows), 2)])
			}
			return nil
		}},
		{"feed.xml", `<?xml version="1.0"?><person>` + marker + `</person>`, XML, func(b []byte) error { return xml.Unmarshal(b, new(struct{})) }},
		{"page.html", `<!doctype html><p>` + marker + strings.Repeat(" ", 200), HTML, nil},
		{"notes.txt", "Note for " + marker + "\nčćžšđ ЉЊЏ\n", Text, nil},
	}
	for _, c := range cases {
		info, out := place(t, c.name, []byte(c.src))
		if info.Kind != c.kind {
			t.Fatalf("%s: kind %s", c.name, info.Kind)
		}
		if len(out) != len(c.src) {
			t.Fatalf("%s: %d bytes, want %d", c.name, len(out), len(c.src))
		}
		if c.check != nil {
			if err := c.check(out); err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, out)
			}
		}
	}
}

func TestBinaryAndEmpty(t *testing.T) {
	raw := append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1, 0}, marker...) // an old Office file
	info, out := place(t, "old.doc", raw)
	if info.Kind != Binary || !bytes.Equal(out, make([]byte, len(raw))) {
		t.Fatalf("info %+v, out %q", info, out)
	}
	info, out = place(t, "empty.txt", nil)
	if info.Kind != Empty || len(out) != 0 {
		t.Fatalf("info %+v", info)
	}
}

func TestLargeBinaryStreams(t *testing.T) {
	// A reader that serves only the head: the rest of a binary is never read.
	const size = 50 << 20
	r := headOnly{size: size}
	var n countWriter
	info, err := Placeholder(context.Background(), r, size, "video.mp4", &n, Options{})
	if err != nil || info.Kind != Binary || n != size {
		t.Fatalf("info %+v, %d bytes, %v", info, n, err)
	}
}

type headOnly struct{ size int64 }

func (h headOnly) ReadAt(p []byte, off int64) (int, error) {
	if off != 0 || len(p) > 8192 {
		return 0, fmt.Errorf("read at %d of %d bytes", off, len(p))
	}
	for i := range p {
		p[i] = byte(i) | 0x80
	}
	return len(p), nil
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) {
	*c += countWriter(len(p))
	return len(p), nil
}

func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// A number of the file out of the range of int (a length, an offset, an
// object number, a rotation) is compared as a number, never converted first.
func TestPDFNumbersOutOfRange(t *testing.T) {
	big := "1" + strings.Repeat("0", 300)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write([]byte("1 " + big + " 3 0 << /Type /Page >>"))
	zw.Close()
	files := []string{
		"%PDF-1.4\n1 0 obj\n<< /Length " + big + " >>\nstream\nxx\nendstream\nendobj\n",
		fmt.Sprintf("%%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode /N 1 /First %s /Length %d >>\nstream\n%s\nendstream\nendobj\n", big, z.Len(), z.Bytes()),
		fmt.Sprintf("%%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode /N 2 /First 6 /Length %d >>\nstream\n%s\nendstream\nendobj\n", z.Len(), z.Bytes()),
		"%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages " + big + " 0 R >>\nendobj\n2 0 obj\n<< /Type /Page /Rotate " + big + " >>\nendobj\n",
	}
	for i, f := range files {
		info, out := place(t, "crafted.pdf", []byte(f))
		if info.Kind != PDF || info.Pages != 1 {
			t.Fatalf("file %d: info %+v", i, info)
		}
		if pages, err := pdfPages(out); err != nil || len(pages) != 1 {
			t.Fatalf("file %d: pages %v %v", i, pages, err)
		}
	}
}

// A broken PDF costs a few passes over its bytes, not a pass per object:
// an unterminated string, stream, hex string or comment is not searched again
// from every "obj" after it.
func TestPDFBrokenIsLinear(t *testing.T) {
	// Linear work grows about 4 times with 4 times the input, quadratic
	// work 16 times. Only the ratio is checked, never a wall-clock limit,
	// so it holds on a slow runner, with the race detector, and under the
	// load of a whole go test ./...: the sizes are measured in turns, each
	// the best of several runs, and a pair that looks quadratic is measured
	// again before it fails. Quadratic work fails every attempt.
	const small, large = 1 << 18, 1 << 20
	for _, unit := range []string{"1 0 obj (", "1 0 obj<<>>stream\n", "1 0 obj <", "1 0 obj [ %"} {
		file := func(size int) []byte {
			data := []byte("%PDF-1.4\n" + strings.Repeat(unit, size/len(unit)))
			if info, _ := place(t, "broken.pdf", data); info.Pages != 1 || info.Fallback == "" {
				t.Fatalf("%q: info %+v", unit, info)
			}
			return data
		}
		s, l := file(small), file(large)
		var ratios []string
		linear := false
		for attempt := 0; attempt < 3 && !linear; attempt++ {
			ts, tl := pdfLinearBest(s, l)
			ratio := float64(tl) / float64(ts)
			ratios = append(ratios, fmt.Sprintf("%.1f (%v, %v)", ratio, ts, tl))
			linear = ratio < 9 // linear: about 4; quadratic: about 16
		}
		if !linear {
			t.Errorf("%q: 1 MiB takes %s times as long as 256 KiB, in 3 attempts: not linear", unit, strings.Join(ratios, ", "))
		}
	}
	// A cancelled context stops the reading.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := []byte("%PDF-1.4\n" + strings.Repeat("1 0 obj (", 1<<17))
	if _, err := Placeholder(ctx, bytes.NewReader(data), int64(len(data)), "a.pdf", io.Discard, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// pdfLinearBest measures Placeholder on the small and the large file in
// turns, and returns the best time of each over several runs: a run slowed
// down by the scheduler or the collector does not count. Each run reads
// the file as many times as the small one needs to take 5 ms, the same
// count for both sizes, so a coarse clock (Windows) never measures a run
// as zero.
func pdfLinearBest(small, large []byte) (time.Duration, time.Duration) {
	read := func(data []byte) {
		Placeholder(context.Background(), bytes.NewReader(data), int64(len(data)), "broken.pdf", io.Discard, Options{})
	}
	reps := 1
	for {
		start := time.Now()
		for range reps {
			read(small)
		}
		if time.Since(start) >= 5*time.Millisecond || reps >= 1<<16 {
			break
		}
		reps *= 2
	}
	run := func(data []byte) time.Duration {
		runtime.GC()
		start := time.Now()
		for range reps {
			read(data)
		}
		return time.Since(start)
	}
	bs, bl := time.Duration(1<<62), time.Duration(1<<62)
	for range 7 {
		bs = min(bs, run(small))
		bl = min(bl, run(large))
	}
	return bs, bl
}

// The object streams of a file inflate to at most maxInflate bytes together.
func TestPDFObjectStreamBudget(t *testing.T) {
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	zw.Write(bytes.Repeat([]byte(" "), 4<<20))
	zw.Close()
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, "%d 0 obj\n<< /Type /ObjStm /Filter /FlateDecode /N 0 /First 0 /Length %d >>\nstream\n", i+1, z.Len())
		b.Write(z.Bytes())
		b.WriteString("\nendstream\nendobj\n")
	}
	work := &pdfWork{ctx: context.Background(), scan: 1 << 30, inflate: 5 << 20}
	pdfObjects(b.Bytes(), work)
	if work.inflate != 0 {
		t.Fatalf("%d bytes of the budget left, want all spent and no more", work.inflate)
	}
}

// panicReader stands for a reader that fails in an unforeseen way.
type panicReader struct{}

func (panicReader) ReadAt([]byte, int64) (int, error) { panic("unforeseen") }

func TestPlaceholderRecovers(t *testing.T) {
	_, err := Placeholder(context.Background(), panicReader{}, 10, "a.bin", io.Discard, Options{})
	if !errors.Is(err, errUnreadable) {
		t.Fatalf("err %v", err)
	}
}

// A narrow image is not slower than a square one of as many pixels: the
// rows go to the compressor in large writes, not one or two per row.
func TestPNGNarrowCost(t *testing.T) {
	// The compressor's cost grows with the number of writes: one or two per
	// row made a narrow image four to seven times slower than a square one
	// with as many pixels. Counting writes holds on any machine, where a
	// time ratio did not on a loaded CI runner.
	for _, c := range []struct{ w, h int }{{1, 2048 * 2048}, {2048, 2048}, {100000, 3}} {
		var cw countingWriter
		if err := pngRows(context.Background(), &cw, c.w, c.h); err != nil {
			t.Fatal(err)
		}
		bytesIn := (c.w + 1) * c.h
		if want := bytesIn/(64<<10) + 2*c.h/max(1, (64<<10)/(c.w+1)) + 4; cw.writes > want {
			t.Errorf("%d×%d: %d writes for %d bytes, want at most %d", c.w, c.h, cw.writes, bytesIn, want)
		}
		if cw.bytes != bytesIn {
			t.Errorf("%d×%d: %d bytes written, want %d", c.w, c.h, cw.bytes, bytesIn)
		}
	}
}

type countingWriter struct{ writes, bytes int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	c.bytes += len(p)
	return len(p), nil
}
