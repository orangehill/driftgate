package files

import (
	"archive/zip"
	"bytes"
	"context"
	"hash/crc32"
	"io"
	"strings"
	"sync"
)

// zipPlan tells a document from a plain archive by its entry names (and the
// "mimetype" entry of an OpenDocument file), and writes a minimal document
// of the same type; any other archive becomes an empty archive, since entry
// names may be personal too.
func zipPlan(r io.ReaderAt, size int64, info *Info) plan {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		info.Fallback = "zip directory not readable"
	} else {
		info.Kind = zipKind(zr)
	}
	parts := officeParts()[info.Kind]
	return func(_ context.Context, w io.Writer) error {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, p := range parts {
			// Stored, without times or data descriptors: the same bytes from
			// any version of the compressor. OpenDocument also requires the
			// mimetype entry first and stored.
			n := uint64(len(p[1]))
			f, err := zw.CreateRaw(&zip.FileHeader{Name: p[0], Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte(p[1])), CompressedSize64: n, UncompressedSize64: n})
			if err != nil {
				return err
			}
			if _, err := io.WriteString(f, p[1]); err != nil {
				return err
			}
		}
		if err := zw.Close(); err != nil {
			return err
		}
		_, err := w.Write(buf.Bytes())
		return err
	}
}

func zipKind(zr *zip.Reader) string {
	has := map[string]bool{}
	for _, f := range zr.File {
		top, _, _ := strings.Cut(f.Name, "/")
		has[top] = true
		if f.Name == "mimetype" && f.UncompressedSize64 <= 128 {
			if rc, err := f.Open(); err == nil {
				b, _ := io.ReadAll(io.LimitReader(rc, 128))
				rc.Close()
				switch strings.TrimSpace(string(b)) {
				case mediaTypes[ODT]:
					return ODT
				case mediaTypes[ODS]:
					return ODS
				case mediaTypes[ODP]:
					return ODP
				}
			}
		}
	}
	if has["[Content_Types].xml"] {
		switch {
		case has["word"]:
			return DOCX
		case has["xl"]:
			return XLSX
		case has["ppt"]:
			return PPTX
		}
	}
	return Zip
}

const (
	xmlDecl    = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	relsNS     = `http://schemas.openxmlformats.org/package/2006/relationships`
	officeRel  = `http://schemas.openxmlformats.org/officeDocument/2006/relationships`
	odfOffice  = `urn:oasis:names:tc:opendocument:xmlns:office:1.0`
	odfTable   = `urn:oasis:names:tc:opendocument:xmlns:table:1.0`
	odfText    = `urn:oasis:names:tc:opendocument:xmlns:text:1.0`
	manifestNS = `urn:oasis:names:tc:opendocument:xmlns:manifest:1.0`
)

func contentTypes(overrides ...string) string {
	s := xmlDecl + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>`
	for i := 0; i < len(overrides); i += 2 {
		s += `<Override PartName="` + overrides[i] + `" ContentType="` + overrides[i+1] + `"/>`
	}
	return s + `</Types>`
}

func rels(target string) string {
	return xmlDecl + `<Relationships xmlns="` + relsNS + `"><Relationship Id="rId1" Type="` + officeRel + `/officeDocument" Target="` + target + `"/></Relationships>`
}

func odf(kind, body string) [][2]string {
	return [][2]string{
		{"mimetype", mediaTypes[kind]},
		{"META-INF/manifest.xml", xmlDecl + `<manifest:manifest xmlns:manifest="` + manifestNS + `" manifest:version="1.2">` +
			`<manifest:file-entry manifest:full-path="/" manifest:version="1.2" manifest:media-type="` + mediaTypes[kind] + `"/>` +
			`<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/></manifest:manifest>`},
		{"content.xml", xmlDecl + `<office:document-content xmlns:office="` + odfOffice + `" xmlns:table="` + odfTable + `" xmlns:text="` + odfText + `" office:version="1.2">` +
			`<office:body>` + body + `</office:body></office:document-content>`},
	}
}

// officeParts are the entries of the minimal document of each type: an empty
// paragraph, an empty sheet, a presentation without slides. An archive that
// is not a known document gets none: an empty archive.
var officeParts = sync.OnceValue(func() map[string][][2]string {
	return map[string][][2]string{
		DOCX: {
			{"[Content_Types].xml", contentTypes("/word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml")},
			{"_rels/.rels", rels("word/document.xml")},
			{"word/document.xml", xmlDecl + `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p/></w:body></w:document>`},
		},
		XLSX: {
			{"[Content_Types].xml", contentTypes(
				"/xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml",
				"/xl/worksheets/sheet1.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml")},
			{"_rels/.rels", rels("xl/workbook.xml")},
			{"xl/workbook.xml", xmlDecl + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="` + officeRel + `">` +
				`<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`},
			{"xl/_rels/workbook.xml.rels", xmlDecl + `<Relationships xmlns="` + relsNS + `"><Relationship Id="rId1" Type="` + officeRel + `/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
			{"xl/worksheets/sheet1.xml", xmlDecl + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData/></worksheet>`},
		},
		// A presentation without a slide master: LibreOffice opens it, PowerPoint
		// may offer to repair it.
		PPTX: {
			{"[Content_Types].xml", contentTypes("/ppt/presentation.xml", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml")},
			{"_rels/.rels", rels("ppt/presentation.xml")},
			{"ppt/presentation.xml", xmlDecl + `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="` + officeRel + `">` +
				`<p:sldSz cx="9144000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`},
		},
		ODT: odf(ODT, `<office:text><text:p/></office:text>`),
		ODS: odf(ODS, `<office:spreadsheet><table:table table:name="Sheet1"><table:table-row><table:table-cell/></table:table-row></table:table></office:spreadsheet>`),
		ODP: odf(ODP, `<office:presentation/>`),
	}
})
