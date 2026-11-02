// Package scan scans a response body that would go into git, as public,
// for personal data and credentials, as a reader of the repository would
// see it: a capture's responses and the bodies of a baseline in git alike.
package scan

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"mime"
	"path"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/har"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/sheet"
)

// Public scans a body a record would put into git for personal data
// and credentials, as a reader of the repository would see it: decoded
// from its Content-Encoding (gzip, deflate), and from gzip whatever the
// headers say (a .gz file served as JSON). A text is scanned as UTF-8
// (text): a textual content type, or UTF-8 without a type or as
// application/octet-stream; a spreadsheet (XLSX) part by part. Anything
// else (an image, a PDF, an archive, a text that cannot be read as UTF-8)
// cannot be scanned: unscanned says why, and the body is stored private
// like one with personal data. The headers are the record's (lower-case
// names).
func Public(d *pii.Detector, headers [][2]string, body []byte) (findings []pii.Finding, unscanned string) {
	if enc := header(headers, "content-encoding"); enc != "" {
		decoded, ok := httpx.Decode(body, enc, httpx.MaxBody)
		if !ok {
			return nil, "Content-Encoding " + enc + " that cannot be decoded"
		}
		body = decoded
	}
	for i := 0; gzipped(body); i++ {
		decoded, ok := httpx.Decode(body, "gzip", httpx.MaxBody)
		if !ok || i == 3 {
			return nil, "gzip data that cannot be decoded"
		}
		body = decoded
	}
	ct := header(headers, "content-type")
	mt, params, _ := mime.ParseMediaType(ct)
	generic := mt == "" || mt == "application/octet-stream"
	typed := !generic && (har.Textual(mt) || strings.Contains(mt, "javascript") || mt == "application/x-www-form-urlencoded")
	if bytes.HasPrefix(body, []byte("PK\x03\x04")) && sheet.IsXLSX(body) { // its type has "xml" in it
		return scanXLSX(d, body)
	}
	if typed || generic {
		t, why := text(body, strings.ToLower(params["charset"]), typed)
		switch {
		case why != "" && typed:
			return nil, why + " (" + mt + ")"
		case why == "":
			return d.ScanResponse(t), ""
		}
	}
	if mt == "" {
		mt = "no content type"
	}
	return nil, "binary (" + mt + ")"
}

// gzipped reports the magic number of gzip (and its only method, deflate).
func gzipped(b []byte) bool { return len(b) >= 3 && b[0] == 0x1f && b[1] == 0x8b && b[2] == 8 }

// text returns a body as the UTF-8 text the patterns read, or why it
// cannot: UTF-16 and UTF-32 are decoded, by the byte order mark or the
// declared charset (their ASCII has NUL bytes the patterns stop at). UTF-8
// is read as it is, without NUL bytes (no text has them). A body with a
// textual type (typed) may also be in a legacy charset that keeps ASCII as
// it is (windows-1250, ISO-8859-2), where an e-mail address or a number
// reads the same: one without control characters, which compressed or
// binary data has. A charset that changes ASCII (UTF-7, EBCDIC) or that is
// not known cannot be read.
func text(b []byte, charset string, typed bool) ([]byte, string) {
	var order binary.ByteOrder
	width := 0
	switch {
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		b = b[3:]
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}):
		b, order, width = b[4:], binary.LittleEndian, 4
	case bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		b, order, width = b[4:], binary.BigEndian, 4
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		b, order, width = b[2:], binary.LittleEndian, 2
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		b, order, width = b[2:], binary.BigEndian, 2
	case strings.HasPrefix(charset, "utf-16"), strings.HasPrefix(charset, "utf-32"), charset == "ucs-2":
		width = 2
		if strings.HasPrefix(charset, "utf-32") {
			width = 4
		}
		// Big endian unless the charset or the first character says
		// otherwise (RFC 2781): Windows writes "utf-16" little endian.
		order = binary.BigEndian
		if strings.HasSuffix(charset, "le") || !strings.HasSuffix(charset, "be") && len(b) >= 2 && b[0] != 0 && b[1] == 0 {
			order = binary.LittleEndian
		}
	case !asciiCharset(charset):
		return nil, "text in charset " + charset + " that cannot be read"
	}
	if width != 0 {
		decoded, ok := fromUTF(b, order, width)
		if !ok {
			return nil, "text that is not valid UTF-" + strconv.Itoa(8*width)
		}
		b = decoded
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return nil, "text with NUL bytes"
	}
	if !utf8.Valid(b) && (!typed || control(b)) {
		return nil, "text that is not valid UTF-8"
	}
	return b, ""
}

// asciiCharset reports a charset (lower case; "" for none) whose text has
// ASCII as it is: UTF-8 and the legacy 8-bit and East Asian ones.
func asciiCharset(cs string) bool {
	switch cs {
	case "", "utf-8", "utf8", "us-ascii", "ascii", "macintosh", "gbk", "gb2312", "gb18030", "big5", "big5-hkscs",
		"shift_jis", "sjis", "windows-31j", "tis-620":
		return true
	}
	for _, p := range []string{"iso-8859-", "iso8859-", "iso_8859-", "latin", "windows-", "cp125", "koi8-", "x-mac-", "euc-", "iso-2022-"} {
		if strings.HasPrefix(cs, p) {
			return true
		}
	}
	return false
}

// control reports a control character other than white space: no text in
// an 8-bit charset has one, nearly every compressed or binary body does.
func control(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' || c == 0x7f {
			return true
		}
	}
	return false
}

// fromUTF decodes UTF-16 (width 2) or UTF-32 (4) to UTF-8; ok is false for
// a truncated body, an unpaired surrogate or a code point out of range.
func fromUTF(b []byte, order binary.ByteOrder, width int) (out []byte, ok bool) {
	if len(b)%width != 0 {
		return nil, false
	}
	out = make([]byte, 0, len(b)/width+len(b)/(2*width)) // mostly ASCII, room for some more
	for i := 0; i < len(b); i += width {
		var r rune
		if width == 4 {
			r = rune(order.Uint32(b[i:]))
		} else if r = rune(order.Uint16(b[i:])); utf16.IsSurrogate(r) {
			if i += 2; i >= len(b) {
				return nil, false
			}
			if r = utf16.DecodeRune(r, rune(order.Uint16(b[i:]))); r == utf8.RuneError {
				return nil, false
			}
		}
		if !utf8.ValidRune(r) {
			return nil, false
		}
		out = utf8.AppendRune(out, r)
	}
	return out, true
}

// scanXLSX scans the text of every XML part of a workbook: the cells
// (shared strings, numbers), comments and the document's properties (its
// author). A part that is not XML (an image) cannot be scanned.
func scanXLSX(d *pii.Detector, body []byte) ([]pii.Finding, string) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, "a workbook that cannot be read"
	}
	var out []pii.Finding
	left := int64(httpx.MaxBody) // decompressed, over all parts
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if ext := path.Ext(f.Name); ext != ".xml" && ext != ".rels" {
			return nil, "a workbook with a part that is not text (" + f.Name + ")"
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "a workbook that cannot be read"
		}
		text, err := io.ReadAll(io.LimitReader(rc, left+1))
		rc.Close()
		if left -= int64(len(text)); err != nil || left < 0 {
			return nil, "a workbook that cannot be read whole"
		}
		out = append(out, d.ScanText("xlsx:"+f.Name, string(text))...)
	}
	return out, ""
}

// header is the first value of a header (names lower case, as recorded).
func header(h [][2]string, name string) string {
	for _, kv := range h {
		if kv[0] == name {
			return kv[1]
		}
	}
	return ""
}
