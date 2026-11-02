package files

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// textPlan writes neutral text of the same length as the original: a fixed
// form of the format (an empty JSON object or array, a CSV header with the
// same number of columns and empty rows, an empty XML or HTML document),
// padded with line breaks; plain text repeats a fixed line. When the fixed
// form is longer than the original, it is written whole.
func textPlan(r io.ReaderAt, size int64, head []byte, info *Info) plan {
	var form string
	pad := []byte("\n")
	switch info.Kind {
	case JSON:
		t := bytes.TrimLeft(bytes.TrimPrefix(head, utf8BOM), " \t\r\n")
		switch {
		case len(t) > 0 && t[0] == '[':
			form = "[]"
		case len(t) > 0 && t[0] == '{':
			form = "{}"
		default:
			form = "0"
		}
	case CSV:
		form = csvForm(r, size)
	case XML:
		form = `<?xml version="1.0" encoding="UTF-8"?>` + "\n<placeholder/>\n"
	case HTML:
		form = "<!DOCTYPE html>\n<html><head><title>placeholder</title></head><body></body></html>\n"
	default:
		pad = []byte("placeholder text\n")
	}
	return func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, form); err != nil {
			return err
		}
		return fill(ctx, w, size-int64(len(form)), pad)
	}
}

// csvForm is a header of neutral column names and empty rows, as many columns
// as the first line of the original has (by its most frequent delimiter
// outside quotes), filling the length of the original.
func csvForm(r io.ReaderAt, size int64) string {
	line := make([]byte, min(size, 64<<10))
	n, _ := r.ReadAt(line, 0)
	line = bytes.TrimPrefix(line[:n], utf8BOM)
	if i := bytes.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	counts := map[byte]int{}
	quoted := false
	for _, c := range line {
		switch {
		case c == '"':
			quoted = !quoted
		case !quoted && (c == ',' || c == ';' || c == '\t'):
			counts[c]++
		}
	}
	delim := byte(',')
	for _, c := range []byte{';', '\t'} {
		if counts[c] > counts[delim] {
			delim = c
		}
	}
	cols := min(counts[delim]+1, 10000)
	names := make([]string, cols)
	for i := range names {
		names[i] = fmt.Sprintf("column%d", i+1)
	}
	header := strings.Join(names, string(delim)) + "\n"
	row := strings.Repeat(string(delim), cols-1) + "\n"
	rows := max(size-int64(len(header)), 0) / int64(len(row))
	// Past a MiB the rest is line breaks, to keep the form small in memory.
	return header + strings.Repeat(row, int(min(rows, int64(1<<20/len(row)))))
}
