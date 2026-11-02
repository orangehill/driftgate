package capture

import (
	"context"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
)

func TestDatabaseHostMustBeAllowed(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGSERVICE", "")
	cases := []struct {
		dsn, allowed string
		remote, ok   bool
	}{
		{"mysql://root:x@127.0.0.1:3306/", "", false, true},
		// Without allow_hosts only the local system: a private address may
		// be production.
		{"mysql://root:x@10.1.2.3:3306/", "", false, false},
		{"mysql://root:x@10.1.2.3:3306/", "10.0.0.0/8", false, true},
		{"mysql://root:x@localhost:3306/", "", false, true},
		{"root:x@unix(/tmp/mysql.sock)/", "", false, true},
		{"mysql://root:x@8.8.8.8:3306/", "", false, false},
		{"mysql://root:x@8.8.8.8:3306/", "", true, true},
		{"postgres://u:p@8.8.4.4:5432/postgres", "", false, false},
		{"postgres://u:p@db.internal:5432/postgres", "db.internal", false, true},
		// The driver connects to host=, not to the host of the URL.
		{"postgres://u:p@127.0.0.1:5432/postgres?host=203.0.113.10", "", false, false},
		{"postgres://u:p@127.0.0.1:5432,203.0.113.10:5432/postgres", "", false, false},
		{"postgres://u:p@/postgres?host=/var/run/postgresql", "", false, true},
	}
	for _, c := range cases {
		driver := "mysql"
		if strings.HasPrefix(c.dsn, "postgres") {
			driver = "postgres"
		}
		t.Setenv("DG_GUARD_DSN", c.dsn)
		cfg := config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
			State: map[string]config.Component{"db": {Driver: driver, DSN: config.Secret{Env: "DG_GUARD_DSN"}, Working: "app", AllowRemote: c.remote}}}
		if c.allowed != "" {
			cfg.Target.AllowHosts = []string{c.allowed}
		}
		p, err := config.New(cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		comps, err := openComponents(context.Background(), p, nil, nil)
		for _, comp := range comps {
			comp.Close()
		}
		if ok := err == nil; ok != c.ok {
			t.Errorf("%s (allow_remote %v): err %v", c.dsn, c.remote, err)
		}
	}
}

// TestDatabaseDSNNeverShown: a DSN that cannot be read is reported without
// the DSN, whose password would reach the console, run.log and the pull
// request comment.
func TestDatabaseDSNNeverShown(t *testing.T) {
	for _, c := range []struct{ driver, dsn string }{
		{"mysql", "mysql://root:s3cr%t@pw@127.0.0.1:3306/"},
		{"postgres", "postgres://app:s3cr%t@pw@127.0.0.1:5432/postgres"},
		{"postgres", "postgres://app:s3cr%zzpw@127.0.0.1:5432/postgres"},
		{"mysql", "root:s3cr%t@pw@tcp(127.0.0.1:3306/"},
	} {
		t.Setenv("DG_GUARD_DSN", c.dsn)
		for _, remote := range []bool{false, true} {
			cfg := config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				State: map[string]config.Component{"db": {Driver: c.driver, DSN: config.Secret{Env: "DG_GUARD_DSN"}, Working: "app", AllowRemote: remote}}}
			p, err := config.New(cfg, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			comps, err := openComponents(context.Background(), p, nil, nil)
			for _, comp := range comps {
				comp.Close()
			}
			if err == nil {
				t.Errorf("%s: accepted", c.dsn)
				continue
			}
			if strings.Contains(err.Error(), "s3cr") || strings.Contains(err.Error(), "pw@") {
				t.Errorf("%s: the error shows the DSN: %v", c.dsn, err)
			}
		}
	}
}
