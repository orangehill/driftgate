package procx

import (
	"strings"
	"testing"
)

// BenchmarkLine: a seed command as configured, a long script and a long
// argv.
func BenchmarkLine(b *testing.B) {
	for _, c := range []struct {
		name string
		argv []string
	}{
		{"seed", []string{"sh", "-c", `mysql -u root -p"$DB_PASSWORD" app < dump.sql && PGPASSWORD=S3cret psql -h h -c "ALTER ROLE x PASSWORD 'y'"`}},
		{"argv 2k", append([]string{"rm"}, strings.Split(strings.Repeat("file.txt ", 2000), " ")...)},
		{"script 2k", []string{"sh", "-c", strings.Repeat("a -p x; ", 2000)}},
	} {
		b.Run(c.name, func(b *testing.B) {
			for range b.N {
				Line(c.argv)
			}
		})
	}
}
