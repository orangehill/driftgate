package pii

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func benchScan(b *testing.B, body []byte) {
	d, _ := New(nil)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		d.Scan(body)
	}
}

// BenchmarkScanJSON is about 4 MB of API response with personal data in
// every tenth record; Clean is the same without any.
func BenchmarkScanJSON(b *testing.B)      { benchScan(b, corpus(5000, true)) }
func BenchmarkScanJSONClean(b *testing.B) { benchScan(b, corpus(5000, false)) }
func BenchmarkScanJSONSmall(b *testing.B) { benchScan(b, corpus(15, false)) }
func BenchmarkScanHTML(b *testing.B)      { benchScan(b, page(20000)) }

// BenchmarkScanStamps is about 3 MB of records made of UUIDs and ISO times.
func BenchmarkScanStamps(b *testing.B) { benchScan(b, stamps(10000)) }

// BenchmarkScanBase64 is a body of long base64-looking tokens that decode to
// nothing printable (hashes, signatures).
func BenchmarkScanBase64(b *testing.B) {
	tok := []byte(`"dGhpcyBpcyBub3QgdGV4dA4AAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHw",`)
	body := append(append([]byte(`{"t":[`), bytes.Repeat(tok, 20000)...), `"x"]}`...)
	benchScan(b, body)
}

// BenchmarkScanBinary is 4 MB of bytes that are neither JSON nor text,
// with a NUL as the second byte (as in an ICO or a TrueType font).
func BenchmarkScanBinary(b *testing.B) {
	body := make([]byte, 4<<20)
	for i := range body {
		body[i] = byte(i * 7919 >> 3)
	}
	body[0], body[1] = 'x', 0
	benchScan(b, body)
}

// The capture scans public responses with ScanResponse, credentials too.
func benchResponse(b *testing.B, body []byte) {
	d, _ := New(nil)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		d.ScanResponse(body)
	}
}

func BenchmarkScanResponseJSON(b *testing.B)      { benchResponse(b, corpus(5000, true)) }
func BenchmarkScanResponseJSONClean(b *testing.B) { benchResponse(b, corpus(5000, false)) }
func BenchmarkScanResponseHTML(b *testing.B)      { benchResponse(b, page(20000)) }
func BenchmarkScanResponseStamps(b *testing.B)    { benchResponse(b, stamps(10000)) }

// BenchmarkScanResponseXML is about 2 MB of XML records, read element by
// element for credentials.
func BenchmarkScanResponseXML(b *testing.B) {
	var s strings.Builder
	s.WriteString(`<?xml version="1.0" encoding="UTF-8"?><items>`)
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&s, `<item id="%d" updated="2024-01-15T10:30:00Z"><title>News from Niš, issue %d</title><link>https://app.test/news/%d?page=2</link><guid>550e8400-e29b-41d4-a716-%012d</guid></item>`, i, i, i, i)
	}
	s.WriteString(`</items>`)
	benchResponse(b, []byte(s.String()))
}
