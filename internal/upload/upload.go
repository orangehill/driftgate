// Package upload builds multipart/form-data bodies for catalog requests that
// send files, and the synthetic files they send when no real file is named:
// a minimal valid file of a kind (CSV, XLSX, PNG, PDF…), the same bytes every
// time, holding no data of anyone.
package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/sheet"
)

// Synthetic kinds.
const (
	CSV  = "csv"
	XLSX = "xlsx"
	TXT  = "txt"
	JSON = "json"
	PNG  = "png"
	JPEG = "jpeg"
	GIF  = "gif"
	PDF  = "pdf"
)

var mediaTypes = map[string]string{
	CSV:  "text/csv",
	XLSX: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	TXT:  "text/plain",
	JSON: "application/json",
	PNG:  "image/png",
	JPEG: "image/jpeg",
	GIF:  "image/gif",
	PDF:  "application/pdf",
}

var extensions = map[string]string{CSV: "csv", XLSX: "xlsx", TXT: "txt", JSON: "json", PNG: "png", JPEG: "jpg", GIF: "gif", PDF: "pdf"}

// Kinds lists the synthetic kinds.
func Kinds() []string {
	out := make([]string, 0, len(mediaTypes))
	for k := range mediaTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Known reports whether kind is a synthetic kind.
func Known(kind string) bool { _, ok := mediaTypes[kind]; return ok }

// MediaType is the content type a file of the kind is sent with.
func MediaType(kind string) string { return mediaTypes[kind] }

// FileName is the name a synthetic file of the kind is sent with.
func FileName(kind string) string { return "upload." + extensions[kind] }

// KindOf picks the synthetic kind that stands in for a file, from its media
// type or its name (an extension or a Laravel mimes: value); "" when none
// fits.
func KindOf(nameOrExt, mediaType string) string {
	mt, _, _ := mime.ParseMediaType(mediaType)
	for k, t := range mediaTypes {
		if mt != "" && mt == t {
			return k
		}
	}
	switch mt {
	case "application/vnd.ms-excel":
		return XLSX
	case "application/csv", "text/comma-separated-values":
		return CSV
	case "image/jpg", "image/pjpeg":
		return JPEG
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext("x."+strings.TrimPrefix(nameOrExt, ".")), "."))
	switch ext {
	case "csv", "tsv":
		return CSV
	case "xlsx", "xls", "xlsm":
		return XLSX
	case "txt", "text", "log":
		return TXT
	case "json":
		return JSON
	case "png":
		return PNG
	case "jpg", "jpeg", "jpe":
		return JPEG
	case "gif":
		return GIF
	case "pdf":
		return PDF
	}
	if strings.HasPrefix(mt, "image/") {
		return PNG
	}
	if strings.HasPrefix(mt, "text/") {
		return TXT
	}
	return ""
}

// Synthetic returns the synthetic file of a kind. Spreadsheets hold one
// header row and one row of neutral values, so an import accepts them.
func Synthetic(kind string) ([]byte, error) {
	rows := [][]string{{"name", "value"}, {"driftgate", "1"}}
	switch kind {
	case CSV:
		return []byte("name,value\ndriftgate,1\n"), nil
	case XLSX:
		return sheet.WriteXLSX(rows), nil
	case TXT:
		return []byte("driftgate\n"), nil
	case JSON:
		return []byte("{\"name\":\"driftgate\"}\n"), nil
	case PNG, JPEG, GIF:
		img := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Gray{Y: 0x80}})
		var b bytes.Buffer
		var err error
		switch kind {
		case PNG:
			err = png.Encode(&b, img)
		case JPEG:
			err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 75})
		default:
			err = gif.Encode(&b, img, nil)
		}
		return b.Bytes(), err
	case PDF:
		return minimalPDF(), nil
	}
	return nil, fmt.Errorf("unknown synthetic file kind %q (%s)", kind, strings.Join(Kinds(), ", "))
}

// minimalPDF is one blank A4 page.
func minimalPDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// Part is one file of a multipart body: Data, or what Open gives, which
// is read twice (to name the boundary, then into the body), so a large file
// is not held besides the body.
type Part struct {
	Field string
	Name  string // file name sent in Content-Disposition
	Type  string // Content-Type of the part
	Data  []byte
	Open  func() (io.ReadCloser, error) // when Data is nil
	Size  int64                         // of what Open gives
}

func (p Part) size() int64 {
	if p.Open != nil {
		return p.Size
	}
	return int64(len(p.Data))
}

// copyTo writes the part's content to w; content that is not Size bytes
// long changed since Size was taken.
func (p Part) copyTo(w io.Writer) error {
	if p.Open == nil {
		_, err := w.Write(p.Data)
		return err
	}
	f, err := p.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, f)
	if err == nil && n != p.Size {
		err = fmt.Errorf("%s changed while it was read (%d bytes, then %d)", p.Name, p.Size, n)
	}
	return err
}

// Encode writes a multipart/form-data body: the fields in order, then the
// files. The boundary is derived from the content, so the same request is
// the same bytes on every capture, and it never occurs in the content. The
// body is written into a buffer sized for it up front, never grown.
func Encode(fields [][2]string, files []Part) (body []byte, contentType string, err error) {
	h := sha256.New()
	for _, f := range fields {
		fmt.Fprintf(h, "%q=%q\n", f[0], f[1])
	}
	size := 256 // the closing boundary, and room to spare
	for _, f := range fields {
		size += 160 + 2*len(f[0]) + len(f[1])
	}
	for _, p := range files {
		fmt.Fprintf(h, "%q;%q;%q;%d\n", p.Field, p.Name, p.Type, p.size())
		if err := p.copyTo(h); err != nil {
			return nil, "", err
		}
		size += 256 + 2*(len(p.Field)+len(p.Name)) + len(p.Type) + int(p.size())
	}
	sum := h.Sum(nil)
	for i := 0; ; i++ {
		s := sha256.Sum256(append(sum, byte(i)))
		boundary := "driftgate-" + hex.EncodeToString(s[:12])
		if contains(fields, files, boundary) {
			continue
		}
		body, found, err := write(fields, files, boundary, size)
		if err != nil {
			return nil, "", err
		}
		if !found {
			return body, "multipart/form-data; boundary=" + boundary, nil
		}
	}
}

// write writes the body with the boundary; found reports content read
// through Open that holds the boundary, so another one is needed.
func write(fields [][2]string, files []Part, boundary string, size int) (body []byte, found bool, err error) {
	b := bytes.NewBuffer(make([]byte, 0, size))
	w := multipart.NewWriter(b)
	if err := w.SetBoundary(boundary); err != nil {
		return nil, false, err
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, false, err
		}
	}
	for _, p := range files {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeQuotes(p.Field), escapeQuotes(p.Name)))
		typ := p.Type
		if typ == "" {
			typ = "application/octet-stream"
		}
		hdr.Set("Content-Type", typ)
		pw, err := w.CreatePart(hdr)
		if err != nil {
			return nil, false, err
		}
		start := b.Len()
		if err := p.copyTo(pw); err != nil {
			return nil, false, err
		}
		if p.Open != nil && bytes.Contains(b.Bytes()[start:], []byte(boundary)) {
			return nil, true, nil
		}
	}
	if err := w.Close(); err != nil {
		return nil, false, err
	}
	return b.Bytes(), false, nil
}

// contains reports s in the fields, the file names or the files' Data; the
// content of a file read through Open is checked as it is written.
func contains(fields [][2]string, files []Part, s string) bool {
	for _, f := range fields {
		if strings.Contains(f[0], s) || strings.Contains(f[1], s) {
			return true
		}
	}
	for _, p := range files {
		if bytes.Contains(p.Data, []byte(s)) || strings.Contains(p.Name, s) {
			return true
		}
	}
	return false
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "%0D", "\n", "%0A")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }
