package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"gopkg.in/yaml.v3"
)

// BenchmarkCaptureRowTracking is a capture of a read-only request and ten
// that each insert a row into t0 of benchSchema, with 2000 more rows, under
// row_tracking: the rows of t0 are read before and after each request
// that writes, and (from fix/row-value on) the values each wrote are
// named under the project key, its column at left out by ignore. record-B is the size of the capture's
// records. On benchServers.
func BenchmarkCaptureRowTracking(b *testing.B) {
	servers := benchServers()
	reqs := []catalog.Request{{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}}
	for i := range 10 {
		reqs = append(reqs, catalog.Request{ID: fmt.Sprintf("post%d", i), Method: "POST", Path: "/", Storage: snapshot.Public})
	}
	cat := &catalog.Catalog{Version: 1, Requests: reqs}
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		b.Run(driver, func(b *testing.B) {
			b.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=")
			p, exec := benchProject(b, driver, dsn)
			rows := make([]string, 2000)
			for r := range rows {
				rows[r] = fmt.Sprintf("('more %d', %d, '2024-01-01 00:00:00')", r, r)
			}
			if err := exec("INSERT INTO t0 (name, n, at) VALUES " + strings.Join(rows, ", ")); err != nil {
				b.Fatal(err)
			}
			if err := Seed(context.Background(), p, SeedOptions{}); err != nil {
				b.Fatal(err)
			}
			sc := p.State["db"]
			if err := yaml.Unmarshal([]byte("[t0: {ignore: [AT, x_*]}]"), &sc.RowTracking); err != nil {
				b.Fatal(err)
			}
			p.State["db"] = sc
			ctx := context.Background()
			var size int64
			for range b.N {
				if _, err := Run(ctx, p, cat, Options{Tag: "bench", Force: true}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			dir, _ := p.StoreDir()
			files, _ := filepath.Glob(filepath.Join(dir, "captures", "bench", "responses", "*.json"))
			for _, f := range files {
				if st, err := os.Stat(f); err == nil {
					size += st.Size()
				}
			}
			b.ReportMetric(float64(size), "record-B")
		})
	}
}
