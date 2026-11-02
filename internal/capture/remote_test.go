package capture

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestRepeatKeepsRateLimitsOnRemoteTargets: the sends of a repeated request
// skip target.rate_limits only on a target inside its allow_hosts.
func TestRepeatKeepsRateLimitsOnRemoteTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	limits := []config.RateLimit{{Match: "/x", PerMinute: 600}} // one send every 100 ms
	for _, tc := range []struct {
		name       string
		allowHosts []string
		remote     bool
	}{
		{"local", nil, false},
		{"remote", []string{"10.0.0.0/8"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &config.Project{Config: config.Config{Target: config.Target{BaseURL: srv.URL, AllowHosts: tc.allowHosts, RateLimits: limits}}}
			req := catalog.Request{ID: "x", Method: "GET", Path: "/x", Repeat: 4, Storage: snapshot.Public}
			s, err := newService(ctx, p, req, true)
			if err != nil {
				t.Fatal(err)
			}
			if s.remote != tc.remote {
				t.Fatalf("remote %v", s.remote)
			}
			r := &runner{services: map[string]*service{"": s}}
			start := time.Now()
			res := r.repeat(ctx, req)
			took := time.Since(start)
			if res.Record.Repeat != "200×4" {
				t.Fatalf("repeat: %+v", res.Record)
			}
			if limited := took >= 250*time.Millisecond; limited != tc.remote {
				t.Fatalf("4 sends took %s; rate limited: %v", took, limited)
			}
		})
	}
}
