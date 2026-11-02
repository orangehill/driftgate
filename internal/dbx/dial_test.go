package dbx

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orangehill/driftgate/internal/httpx"
)

// rebinding makes db.rebind.test (a name no real resolver knows) resolve
// to 127.0.0.1 until the returned flag is set, and to a public address
// after it, as a DNS server with a short TTL can.
func rebinding(t *testing.T) *atomic.Bool {
	var moved atomic.Bool
	lookupIP = func(_ context.Context, _, host string) ([]net.IP, error) {
		if host != "db.rebind.test" {
			return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		}
		if !moved.Load() {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("192.0.2.10")}, nil
	}
	t.Cleanup(func() { lookupIP = nil })
	return &moved
}

// TestAllowDialChecksTheAddressDialled: the name passes AllowHost, then
// resolves elsewhere; the connection is refused at the dial, before any
// packet goes to the other address.
func TestAllowDialChecksTheAddressDialled(t *testing.T) {
	ctx := context.Background()
	moved := rebinding(t)
	if err := AllowHost(ctx, nil, "db.rebind.test", "x"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	moved.Store(true)
	dial, err := AllowDial(nil, "allow_remote: true")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(ctx, "tcp", "db.rebind.test:3306"); !errors.Is(err, httpx.ErrHostNotAllowed) || !strings.Contains(err.Error(), "allow_remote: true") {
		t.Fatalf("dial after rebinding: %v", err)
	}
	// An allowed address is dialled as it was checked.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := dial(ctx, "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// A host one list names may resolve anywhere; another list allows it.
	dial, _ = AllowDial([][]string{{"10.0.0.0/8"}, {"localhost"}}, "x")
	if c, err := dial(ctx, "tcp", strings.Replace(l.Addr().String(), "127.0.0.1", "localhost", 1)); err != nil && errors.Is(err, httpx.ErrHostNotAllowed) {
		t.Fatalf("named host: %v", err)
	} else if c != nil {
		c.Close()
	}
}

// TestDriversDialThroughAllowDial: the MySQL and PostgreSQL drivers connect
// through the dial function, not by resolving the name themselves: the
// same server is refused when its name now resolves elsewhere.
func TestDriversDialThroughAllowDial(t *testing.T) {
	type server struct{ driver, dsn string }
	var servers []server
	for _, dsn := range strings.Split(os.Getenv("DRIFTGATE_TEST_MYSQL"), ",") {
		if dsn != "" {
			servers = append(servers, server{"mysql", dsn})
		}
	}
	if dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES"); dsn != "" {
		servers = append(servers, server{"postgres", dsn})
	}
	if len(servers) == 0 {
		t.Skip("DRIFTGATE_TEST_MYSQL and DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	for _, s := range servers {
		u, err := url.Parse(s.dsn)
		if err != nil || u.Hostname() != "127.0.0.1" {
			t.Skipf("%s is not on 127.0.0.1", u.Host)
		}
		t.Run(s.driver+"-"+u.Port(), func(t *testing.T) {
			u.Host = "db.rebind.test:" + u.Port()
			open := func() error {
				dial, err := AllowDial(nil, "x")
				if err != nil {
					t.Fatal(err)
				}
				var db interface {
					PingContext(context.Context) error
					Close() error
				}
				if s.driver == "mysql" {
					db, err = OpenMySQL(u.String(), "", dial)
				} else {
					db, err = OpenPostgres(u.String(), "", dial)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				return db.PingContext(ctx)
			}
			moved := rebinding(t)
			// Only the stub knows the name: the connection works only when
			// the driver dials through AllowDial.
			if err := open(); err != nil {
				t.Fatalf("allowed: %v", err)
			}
			moved.Store(true)
			if err := open(); err == nil || (!errors.Is(err, httpx.ErrHostNotAllowed) && !strings.Contains(err.Error(), "host not allowed")) {
				t.Fatalf("after rebinding: %v", err)
			}
		})
	}
}
