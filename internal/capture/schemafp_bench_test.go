package capture

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/state"
)

// BenchmarkSchemaFingerprint is one SchemaFingerprint of benchSchema, of the
// pristine copy and of the working database: what checkSchema reads per
// component before the first restore (both) and checkPrepared after
// prepare (the working one). Servers as BenchmarkCaptureSchema.
func BenchmarkSchemaFingerprint(b *testing.B) {
	servers := map[string]string{config.SQLite: ""}
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		servers[config.MySQL], _, _ = strings.Cut(v, ",")
	}
	if v := os.Getenv("DRIFTGATE_TEST_POSTGRES"); v != "" {
		servers[config.Postgres] = v
	}
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		b.Run(driver, func(b *testing.B) {
			p, _ := benchProject(b, driver, dsn)
			comps, err := openComponents(context.Background(), p, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer comps[0].Close()
			s := comps[0].(state.Schemer)
			for _, side := range []struct {
				name     string
				pristine bool
			}{{"pristine", true}, {"working", false}} {
				b.Run(side.name, func(b *testing.B) {
					for range b.N {
						if f, err := s.SchemaFingerprint(context.Background(), side.pristine); err != nil || len(f) < benchTables {
							b.Fatal(len(f), err)
						}
					}
				})
			}
		})
	}
}

// BenchmarkCheckSchema is the schema check before a capture's first
// restore on benchSchema, unchanged since the seed: cached, with the
// pristine schema the seed recorded (one read, of the working database);
// live, without it (both read). Servers as BenchmarkCaptureSchema.
func BenchmarkCheckSchema(b *testing.B) {
	servers := map[string]string{config.SQLite: ""}
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		servers[config.MySQL], _, _ = strings.Cut(v, ",")
	}
	if v := os.Getenv("DRIFTGATE_TEST_POSTGRES"); v != "" {
		servers[config.Postgres] = v
	}
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		b.Run(driver, func(b *testing.B) {
			p, _ := benchProject(b, driver, dsn)
			tr, err := newTracker(context.Background(), p, nil, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer tr.close()
			cached := tr.schemas
			for _, mode := range []string{"cached", "live"} {
				b.Run(mode, func(b *testing.B) {
					tr.schemas = cached
					if mode == "live" {
						tr.schemas = states{}
					}
					for range b.N {
						if err := tr.checkSchema(context.Background()); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
