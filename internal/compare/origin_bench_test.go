package compare

import (
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// originRecords are n responses of a CMS list, each with asset URLs of its
// origin and one value that differs between the two sides.
func originRecords(n int, origin, title string) []Side {
	out := make([]Side, n)
	for i := range out {
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for j := 0; j < 10; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%d,"title":"%s %d","image":"%s/storage/assets/%d-%d.jpg","thumb":"%s\/storage\/thumbs\/%d.jpg"}`,
				j, title, i, origin, i, j, strings.ReplaceAll(origin, "/", `\/`), j)
		}
		b.WriteString(`],"links":{"next":"` + origin + `/api/news?page=2"}}`)
		out[i] = side(200, [][2]string{{"content-type", "application/json"}, {"location", origin + "/x"}}, b.String())
	}
	return out
}

func BenchmarkOrigins(b *testing.B) {
	allow := allowlist.Empty()
	same := originRecords(600, "http://localhost:8080", "a")
	sameCur := originRecords(600, "http://localhost:8080", "b")
	other := originRecords(600, "http://localhost:8081", "b")
	run := func(b *testing.B, base, cur []Side, bo, co []string) {
		for i := 0; i < b.N; i++ {
			for k := range base {
				x, y := base[k], cur[k]
				x.Origins, y.Origins = bo, co
				One("req", x, y, allow, LevelValues)
			}
		}
	}
	o8080, o8081 := []string{"http://localhost:8080"}, []string{"http://localhost:8081"}
	b.Run("same-origin/none", func(b *testing.B) { run(b, same, sameCur, nil, nil) })
	b.Run("same-origin/recorded", func(b *testing.B) { run(b, same, sameCur, o8080, o8080) })
	b.Run("other-origin/none", func(b *testing.B) { run(b, same, other, nil, nil) })
	b.Run("other-origin/recorded", func(b *testing.B) { run(b, same, other, o8080, o8081) })
}
