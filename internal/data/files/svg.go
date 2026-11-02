package files

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
)

// svgRoot reads the root element of an SVG document, from at most the first
// MiB; ok is false when the document is not SVG.
func svgRoot(r io.ReaderAt, size int64) (attrs []xml.Attr, ok bool) {
	d := xml.NewDecoder(io.LimitReader(io.NewSectionReader(r, 0, size), 1<<20))
	d.Strict = false
	for {
		tok, err := d.RawToken()
		if err != nil {
			return nil, false
		}
		if el, isStart := tok.(xml.StartElement); isStart {
			return el.Attr, el.Name.Local == "svg"
		}
	}
}

func isSVG(r io.ReaderAt, size int64) bool {
	_, ok := svgRoot(r, size)
	return ok
}

var svgLength = lazy.Regexp(`^\s*([+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?)\s*(px|pt|pc|mm|cm|in|em|ex|%)?\s*$`)

// svgPlan keeps the root's width, height and viewBox when they are plain
// numbers; the placeholder is a grey rectangle over the view box.
func svgPlan(r io.ReaderAt, size int64, info *Info) plan {
	attrs, _ := svgRoot(r, size)
	var width, height string
	var box []float64
	for _, a := range attrs {
		if a.Name.Space != "" {
			continue
		}
		switch a.Name.Local {
		case "width", "height":
			m := svgLength().FindStringSubmatch(a.Value)
			if m == nil {
				continue
			}
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil || math.IsInf(v, 0) {
				continue
			}
			s := strconv.FormatFloat(v, 'f', -1, 64) + m[2]
			px := 0
			if (m[2] == "" || m[2] == "px") && v == math.Trunc(v) && v <= math.MaxInt32 {
				px = int(v)
			}
			if a.Name.Local == "width" {
				width, info.Width = s, px
			} else {
				height, info.Height = s, px
			}
		case "viewBox":
			f := strings.FieldsFunc(a.Value, func(c rune) bool { return c == ',' || c == ' ' || c == '\t' || c == '\n' || c == '\r' })
			if len(f) != 4 {
				continue
			}
			box = box[:0]
			for _, s := range f {
				v, err := strconv.ParseFloat(s, 64)
				if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
					box = nil
					break
				}
				box = append(box, v)
			}
		}
	}
	if attrs == nil {
		info.Fallback = "svg root not readable"
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg"`)
	if width != "" {
		fmt.Fprintf(&b, ` width="%s"`, width)
	}
	if height != "" {
		fmt.Fprintf(&b, ` height="%s"`, height)
	}
	if len(box) == 4 {
		fmt.Fprintf(&b, ` viewBox="%s %s %s %s"><rect x="%[1]s" y="%[2]s" width="%[3]s" height="%[4]s" fill="#808080"/></svg>`+"\n",
			num(box[0]), num(box[1]), num(box[2]), num(box[3]))
	} else {
		b.WriteString(`><rect width="100%" height="100%" fill="#808080"/></svg>` + "\n")
	}
	out := b.String()
	return func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, out)
		return err
	}
}
