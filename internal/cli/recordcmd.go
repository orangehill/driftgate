package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/har"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/proxy"
)

// defaultListen is where record listens unless --listen says otherwise.
const defaultListen = "127.0.0.1:8088"

func recordCmd(args []string, out io.Writer) (int, error) {
	ctx, stop := signalContext(nil)
	defer stop()
	return record(ctx, args, out, nil)
}

// record runs the recording proxy until ctx ends, then lists the new catalog
// entries and with --write adds them. ready, when set, gets the address the
// proxy listens on (tests listen on port 0).
func record(ctx context.Context, args []string, out io.Writer, ready func(addr string)) (int, error) {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	listen := fs.String("listen", defaultListen, "")
	write := fs.Bool("write", false, "")
	anyInterface := fs.Bool("allow-non-loopback", false, "")
	var targetName once
	fs.Var(&targetName, "target", "")
	var include, exclude names
	fs.Var(&include, "include", "")
	fs.Var(&exclude, "exclude", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("record")
	}
	if !proxy.Loopback(*listen) && !*anyInterface {
		return 0, configErr("--listen %s is not a loopback address: the proxy forwards requests to the service as they come, "+
			"with no sign-in of its own, so anyone who can reach it uses the service as the developer; it listens on 127.0.0.1 only "+
			"(pass --allow-non-loopback to listen there anyway, on a network you trust)", *listen)
	}
	p, cat, _, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	target, err := p.TargetFor(targetName.v)
	if err != nil {
		return 0, configErr("%v (pass --target)", err)
	}
	if target.BaseURL == "" {
		return 0, configErr("target.base_url is not set; record forwards to it")
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil || base.Host == "" {
		return 0, configErr("target.base_url %q is not a URL", target.BaseURL)
	}
	allow, err := httpx.NewAllow(target.AllowHosts, false)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if err := allow.Check(ctx, base.Hostname()); err != nil {
		if errors.Is(err, httpx.ErrHostNotAllowed) {
			return 0, configErr("target.base_url: %v; record never forwards to a host outside target.allow_hosts", err)
		}
		return 0, envErr(fmt.Errorf("target.base_url: %w", err))
	}
	service := p.Canonical(targetName.v)
	opts, inv, err := candidateOptions(ctx, p, service, out)
	if err != nil {
		return 0, err
	}
	b, err := har.NewBuilder(target.BaseURL, opts)
	if err != nil {
		return 0, configErr("%v", err)
	}
	pr := newProposer(p, cat, b, inv, service, "rec", "record")
	skip := func(path string) bool {
		for _, g := range exclude {
			if glob.Match(g, path) {
				return true
			}
		}
		if len(include) == 0 {
			return false
		}
		for _, g := range include {
			if glob.Match(g, path) {
				return false
			}
		}
		return true
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return 0, envErr(err)
	}
	defer ln.Close()
	// The proxy answers for the --listen host with the port it got (port 0
	// in tests).
	listenHost, _, _ := net.SplitHostPort(*listen)
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	var mu sync.Mutex
	var props []*proposal
	seen := map[string]bool{}
	requests := 0
	unverified := 0 // beyond loopback: no sign of a page of the proxy's own address
	handler, err := proxy.New(proxy.Options{Target: target.BaseURL, Listen: net.JoinHostPort(listenHost, port), Allow: allow, Skip: skip, Record: func(e proxy.Exchange) {
		mu.Lock()
		defer mu.Unlock()
		if e.Unverified {
			unverified++
			return
		}
		label := e.Entry.Method + " " + relPath(base, e.Entry.URL)
		if e.Streamed != "" {
			if !seen["stream "+label] {
				seen["stream "+label] = true
				fmt.Fprintf(out, "  passed through, not recorded: %s (%s)\n", label, e.Streamed)
			}
			return
		}
		requests++
		c, note := b.Add(e.Entry)
		if note != nil {
			if k := "note " + note.Request + note.Reason; !seen[k] {
				seen[k] = true
				fmt.Fprintf(out, "  skipped %s: %s\n", note.Request, note.Reason)
			}
			return
		}
		if c == nil {
			return
		}
		prop := pr.propose(c)
		if prop == nil {
			return
		}
		props = append(props, prop)
		fmt.Fprintf(out, "  new    %-6s %s → %s\n", c.Method, c.Path, prop.request.ID)
		if e.Large {
			fmt.Fprintf(out, "         its body was over %d MiB or not sent whole and is not recorded; add it by hand\n", proxy.MaxRecorded>>20)
		}
	}})
	if err != nil {
		return 0, configErr("%v", err)
	}
	// Every request runs on a context that is cancelled on stop, so one
	// waiting for a slow service, a stream or a websocket ends too; inflight
	// waits for their handlers.
	reqCtx, abort := context.WithCancel(context.Background())
	defer abort()
	var gate sync.RWMutex
	var inflight sync.WaitGroup
	closed := false
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gate.RLock()
			if closed {
				gate.RUnlock()
				http.Error(w, "driftgate record is stopping", http.StatusServiceUnavailable)
				return
			}
			inflight.Add(1)
			gate.RUnlock()
			defer inflight.Done()
			handler.ServeHTTP(w, r)
		}),
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 30 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	fmt.Fprintf(out, "recording %s through http://%s — point the browser or the E2E suite there; Ctrl-C to stop\n", target.BaseURL, ln.Addr())
	fmt.Fprintln(out, "(requests only: no response, cookie or token is kept; event streams and websockets pass through unrecorded)")
	if ready != nil {
		ready(ln.Addr().String())
	}
	select {
	case <-ctx.Done():
	case err := <-served:
		return 0, envErr(err)
	}
	// Requests in flight get a moment to finish; open streams (an event
	// stream, a websocket) do not end by themselves and are cut.
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(stopCtx)
	abort()
	srv.Close()
	gate.Lock()
	closed = true
	gate.Unlock()
	inflight.Wait()
	handler.CloseIdleConnections()

	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(out, "\n%d requests recorded, %d new catalog entries\n", requests, len(props))
	if unverified > 0 {
		fmt.Fprintf(out, "%d requests passed through, not recorded: on an address that is not loopback only a request from a page opened through the proxy "+
			"(by its Origin, Referer or Sec-Fetch-Site) is recorded; record on 127.0.0.1 for other clients\n", unverified)
	}
	fresh := 0
	var added []catalog.Request
	for _, prop := range props {
		prop.settle() // a later response may have held personal data
		r := prop.request
		added = append(added, r)
		var info []string
		switch {
		case prop.route == "":
			info = append(info, "no route in routes.json")
		case prop.fresh:
			fresh++
			info = append(info, "route "+prop.route+", uncovered before")
		default:
			info = append(info, "route "+prop.route)
		}
		if r.Identity != "" {
			info = append(info, "identity "+r.Identity)
		} else if prop.signedIn {
			info = append(info, "signed in; set identity")
		}
		info = append(info, r.Storage)
		fmt.Fprintf(out, "  %s  %s %s  (%s)\n", r.ID, r.Method, r.Path, strings.Join(info, ", "))
	}
	if fresh > 0 {
		fmt.Fprintf(out, "%d routes uncovered before are now reached\n", fresh)
	}
	if !*write || len(added) == 0 {
		if len(added) > 0 {
			fmt.Fprintln(out, "record again with --write to add them to the catalog")
		}
		return ExitClean, nil
	}
	current, err := catalog.Load(p.Layout.Catalog)
	if err != nil {
		return 0, configErr("%v", err)
	}
	current.Requests = append(current.Requests, added...)
	if err := current.Validate(); err != nil {
		return 0, configErr("%v", err)
	}
	if err := current.Save(p.Layout.Catalog); err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "added to %s; run driftgate capture (or run) to record their responses on a restored state\n", p.Layout.Catalog)
	return ExitClean, nil
}

// relPath is the path of a forwarded URL relative to the service, as the
// live output prints it: without the query and with a token or personal
// data in the path masked (har.MaskPath).
func relPath(base *url.URL, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "?"
	}
	p := strings.TrimPrefix(u.Path, strings.TrimRight(base.Path, "/"))
	if p == "" {
		p = "/"
	}
	return har.MaskPath(p)
}
