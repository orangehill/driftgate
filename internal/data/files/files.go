// Package files makes placeholders for uploaded files, so that a copy of an
// application's storage carries none of the original content. A placeholder
// has the same name, the same type (detected from the content, not from the
// extension) and the same dimensions or page sizes, with neutral content: a
// flat grey image, blank pages, an empty document.
//
// What is read from the original is only the structure the placeholder needs,
// listed per type on Placeholder; no byte of the original content is copied
// into the output. The output is deterministic: the same input gives the same
// bytes (no timestamps, fixed encoder settings).
package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Kinds of file.
const (
	PNG    = "png"
	JPEG   = "jpeg"
	GIF    = "gif"
	WebP   = "webp"
	SVG    = "svg"
	PDF    = "pdf"
	DOCX   = "docx"
	XLSX   = "xlsx"
	PPTX   = "pptx"
	ODT    = "odt"
	ODS    = "ods"
	ODP    = "odp"
	Zip    = "zip"
	JSON   = "json"
	CSV    = "csv"
	XML    = "xml"
	HTML   = "html"
	Text   = "text"
	Binary = "binary"
	Empty  = "empty"
)

var mediaTypes = map[string]string{
	PNG:    "image/png",
	JPEG:   "image/jpeg",
	GIF:    "image/gif",
	WebP:   "image/webp",
	SVG:    "image/svg+xml",
	PDF:    "application/pdf",
	DOCX:   "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	XLSX:   "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	PPTX:   "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	ODT:    "application/vnd.oasis.opendocument.text",
	ODS:    "application/vnd.oasis.opendocument.spreadsheet",
	ODP:    "application/vnd.oasis.opendocument.presentation",
	Zip:    "application/zip",
	JSON:   "application/json",
	CSV:    "text/csv",
	XML:    "application/xml",
	HTML:   "text/html",
	Text:   "text/plain",
	Binary: "application/octet-stream",
	Empty:  "application/octet-stream",
}

// Limits.
const (
	DefaultMaxSize   = 256 << 20 // bytes of the original
	DefaultMaxPixels = 1 << 28   // width × height of an image
)

// ErrTooLarge means a file is over a limit; no placeholder is written.
var ErrTooLarge = errors.New("over the size limit")

// errUnreadable means a reader failed on a file it could not make sense of.
var errUnreadable = errors.New("not readable")

// Options limit the work done for one file. Zero values take the defaults.
type Options struct {
	MaxSize   int64 // larger originals are refused with ErrTooLarge
	MaxPixels int64 // images with more pixels are refused with ErrTooLarge
}

func (o Options) maxSize() int64 {
	if o.MaxSize > 0 {
		return o.MaxSize
	}
	return DefaultMaxSize
}

func (o Options) maxPixels() int64 {
	if o.MaxPixels > 0 {
		return o.MaxPixels
	}
	return DefaultMaxPixels
}

// Info describes a placeholder; it never holds content of the original.
type Info struct {
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`             // of the original
	Width     int    `json:"width,omitempty"`  // images; SVG only when given in pixels
	Height    int    `json:"height,omitempty"` // images; SVG only when given in pixels
	Pages     int    `json:"pages,omitempty"`  // PDF
	// Fallback says why the placeholder has the right type but not the
	// original's dimensions or pages (a header that could not be read).
	Fallback string `json:"fallback,omitempty"`
}

// plan writes a placeholder once the original has been read.
type plan func(ctx context.Context, w io.Writer) error

// PlaceholderFile writes the placeholder of the file at path to dst.
func PlaceholderFile(ctx context.Context, path string, dst io.Writer, opt Options) (Info, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	if !st.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%s: not a regular file", path)
	}
	return Placeholder(ctx, f, st.Size(), filepath.Base(path), dst, opt)
}

// Placeholder writes the placeholder of an original of the given size and
// file name to dst. The name is used only for its extension, to tell text
// formats apart (CSV, JSON); binary formats are known by their content.
//
// What is read from the original, per kind:
//   - PNG: the IHDR width and height (bytes 16–23).
//   - JPEG: the marker segment headers up to the first SOFn, and its width
//     and height; segment contents (EXIF, ICC, comments) are skipped by
//     length, not read.
//   - GIF: the logical screen width and height (bytes 6–9).
//   - WebP: the first chunk header and the width and height of its VP8,
//     VP8L or VP8X header.
//   - SVG: the width, height and viewBox attributes of the root element, kept
//     only when they are plain numbers (with a unit) and written anew.
//   - PDF: the object structure (dictionaries, arrays, numbers, names and
//     references, from object streams too); strings and content streams are
//     skipped. Of that, only the page tree is used: the page count and each
//     page's MediaBox, CropBox and Rotate. A file broken so that reading it
//     would take more than a few passes over its bytes gets a single page.
//   - ZIP (and OOXML, ODF): the entry names of the central directory and the
//     content of a short "mimetype" entry, to tell the document type.
//   - Text: the first 8 KiB, to tell it is text; the first character (JSON
//     object or array); for CSV the delimiter count of the first line.
//   - Anything else: the first bytes, to tell it is none of the above.
//
// A crafted file that makes a reader fail in an unforeseen way (a panic) is
// an error, as an unreadable file is; the run goes on.
func Placeholder(ctx context.Context, r io.ReaderAt, size int64, name string, dst io.Writer, opt Options) (info Info, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%w: %v", errUnreadable, v)
		}
	}()
	if size > opt.maxSize() {
		return Info{Size: size}, fmt.Errorf("%d bytes: %w", size, ErrTooLarge)
	}
	head := make([]byte, min(size, 8192))
	if _, err := r.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return Info{Size: size}, err
	}
	info = Info{Kind: detect(r, size, head, name), Size: size}
	var p plan
	switch info.Kind {
	case PNG, JPEG, GIF, WebP:
		p, err = rasterPlan(r, size, &info, opt)
	case SVG:
		p = svgPlan(r, size, &info)
	case PDF:
		p, err = pdfPlan(ctx, r, size, &info)
	case Zip:
		p = zipPlan(r, size, &info)
	case JSON, CSV, XML, HTML, Text:
		p = textPlan(r, size, head, &info)
	case Empty:
		p = func(context.Context, io.Writer) error { return nil }
	default:
		p = func(ctx context.Context, w io.Writer) error { return fill(ctx, w, size, nil) }
	}
	info.MediaType = mediaTypes[info.Kind]
	if err != nil {
		return info, err
	}
	if err := ctx.Err(); err != nil {
		return info, err
	}
	return info, p(ctx, dst)
}

// detect tells the kind of a file by its content; the extension only tells
// text formats apart.
func detect(r io.ReaderAt, size int64, head []byte, name string) string {
	switch {
	case size == 0:
		return Empty
	case bytes.HasPrefix(head, pngMagic):
		return PNG
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return JPEG
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return GIF
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return WebP
	case bytes.Contains(head[:min(len(head), 1024)], []byte("%PDF-")):
		return PDF
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return Zip
	case !isText(head, size):
		return Binary
	}
	ext := strings.ToLower(filepath.Ext(name))
	t := bytes.TrimLeft(bytes.TrimPrefix(head, utf8BOM), " \t\r\n")
	if len(t) > 0 && t[0] == '<' {
		if isSVG(r, size) {
			return SVG
		}
		lower := strings.ToLower(string(t[:min(len(t), 64)]))
		switch {
		case ext == ".html" || ext == ".htm" || strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html"):
			return HTML
		case ext == ".xml" || strings.HasPrefix(lower, "<?xml"):
			return XML
		}
	}
	switch ext {
	case ".csv", ".tsv":
		return CSV
	case ".json", ".geojson":
		return JSON
	case "", ".js", ".map":
		if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			return JSON
		}
	}
	return Text
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// isText reports whether the head is UTF-8 without control characters other
// than tab, line breaks and form feed. A rune cut at the end of the head of a
// longer file is allowed.
func isText(head []byte, size int64) bool {
	for i := 0; i < len(head); {
		c, n := utf8.DecodeRune(head[i:])
		if c == utf8.RuneError && n <= 1 {
			if int64(len(head)) < size && len(head)-i < utf8.UTFMax && !utf8.FullRune(head[i:]) {
				return true
			}
			return false
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' || c == 0x7F {
			return false
		}
		i += n
	}
	return true
}

// fill writes n bytes repeating pattern; zero bytes when pattern is nil.
func fill(ctx context.Context, w io.Writer, n int64, pattern []byte) error {
	buf := make([]byte, 64<<10)
	if pattern != nil {
		for i := range buf {
			buf[i] = pattern[i%len(pattern)]
		}
		// Keep the pattern aligned from one buffer to the next.
		buf = buf[:len(buf)-len(buf)%len(pattern)]
	}
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := min(n, int64(len(buf)))
		if _, err := w.Write(buf[:k]); err != nil {
			return err
		}
		n -= k
	}
	return nil
}
