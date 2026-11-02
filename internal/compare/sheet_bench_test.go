package compare

import (
	"strconv"
	"testing"

	"github.com/orangehill/driftgate/internal/sheet"
)

// BenchmarkXLSX compares two exports of 5000 rows and 6 columns whose first
// cell (the time of the export) differs, allow-listed.
func BenchmarkXLSX(b *testing.B) {
	rows := func(stamp string) [][]string {
		out := [][]string{{stamp}}
		for i := range 5000 {
			n := strconv.Itoa(i)
			out = append(out, []string{n, "Name " + n, "user" + n + "@example.com", "12.5", "RS", "2026-10-29"})
		}
		return out
	}
	typ := [][2]string{{"content-type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}
	base := side(200, typ, string(sheet.WriteXLSX(rows("Exported 10:00"))))
	cur := side(200, typ, string(sheet.WriteXLSX(rows("Exported 11:11"))))
	allow := manyRules(b, 0, false)
	allow.Body[0].Path = "xlsx:Sheet1!A1"
	allow.Body[0].Format = `Exported \d+:\d+`
	if err := allow.Validate(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		One("req", base, cur, allow, LevelValues)
	}
}
