package capture

import (
	"context"
	"io"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
)

// ProbeOptions say which services the throughput probe measures.
type ProbeOptions struct {
	Services    []string // canonical names
	Requests    int      // read-only requests sent at each setting
	Max         int      // the highest setting tried
	AllowRemote bool
	Out, Log    io.Writer
	Env         []string
}

// Throughput is how long the probe requests took at one setting.
type Throughput struct {
	Concurrency int
	Requests    int
	Duration    time.Duration
}

// Probe measures, for each service, how long its first read-only requests
// take at concurrency 1, 2, 4 … up to Max, on the restored and prepared
// state, after one unmeasured pass that warms the application's caches.
// The services are measured one after another and the responses are
// dropped. Requests that take a value from another response or upload
// files are left out. The state is left as the requests leave it: the
// capture that follows starts with a restore.
func Probe(ctx context.Context, p *config.Project, cat *catalog.Catalog, opt ProbeOptions) (map[string][]Throughput, error) {
	defer httpx.FreePool()
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	readonly, _ := plan(cat, "")
	wanted := map[string]bool{}
	for _, s := range opt.Services {
		wanted[s] = true
	}
	byService := map[string][]catalog.Request{}
	services := map[string]*service{}
	for _, req := range readonly {
		req.Target = p.Canonical(req.Target)
		if !wanted[req.Target] || req.IsCommand() || len(req.ParamsFrom) > 0 || req.Multipart != nil || len(byService[req.Target]) >= opt.Requests {
			continue
		}
		if _, ok := services[req.Target]; !ok {
			s, err := newService(ctx, p, req, opt.AllowRemote)
			if err != nil {
				return nil, err
			}
			services[req.Target] = s
		}
		byService[req.Target] = append(byService[req.Target], req)
	}
	out := map[string][]Throughput{}
	if len(services) == 0 {
		return out, nil
	}
	tr, err := newTracker(ctx, p, opt.Env, opt.Out, opt.Log)
	if err != nil {
		return nil, err
	}
	defer tr.close()
	r := &runner{p: p, services: services, out: opt.Out, log: opt.Log, env: opt.Env, tr: tr}
	if err := tr.restore(ctx); err != nil {
		return nil, err
	}
	if err := r.prepare(ctx); err != nil {
		return nil, err
	}
	for _, name := range opt.Services {
		reqs := byService[name]
		if len(reqs) < 2 {
			continue
		}
		s := services[name]
		measure := func(n int) (time.Duration, error) {
			s.slots = make(chan struct{}, n)
			start := time.Now()
			err := r.parallel(ctx, reqs, n, nil)
			return time.Since(start), err
		}
		if _, err := measure(1); err != nil {
			return nil, err
		}
		for n := 1; n <= opt.Max && n <= len(reqs); n *= 2 {
			d, err := measure(n)
			if err != nil {
				return nil, err
			}
			out[name] = append(out[name], Throughput{Concurrency: n, Requests: len(reqs), Duration: d})
		}
	}
	return out, ctx.Err()
}

// Choose picks the smallest setting whose throughput is within 10% of the
// best one: more requests at once only where they pay, since every extra
// one is a chance for an effect of the order.
func Choose(runs []Throughput) int {
	best := time.Duration(0)
	for _, t := range runs {
		if best == 0 || t.Duration < best {
			best = t.Duration
		}
	}
	for _, t := range runs {
		// Requests per second within 10% of the best: d ≤ best / 0.9.
		if t.Duration*9 <= best*10 {
			return t.Concurrency
		}
	}
	return 1
}
