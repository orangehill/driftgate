package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/generate"
	"github.com/orangehill/driftgate/internal/har"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// har import and record feed one pipeline (har.Builder) with the same
// options, and turn its candidates into catalog requests the same way
// (proposer): the same traffic gives the same entries, whether it comes
// from a HAR file or through the recording proxy.

// candidateOptions are the pipeline options of a service: its routes from
// the inventory, so paths become routes, and the configured identities, so
// a request that carries one's token gets that identity. Tokens that cannot
// be loaded only leave identities unrecognised; they are never written.
func candidateOptions(ctx context.Context, p *config.Project, target string, out io.Writer) (har.Options, *routes.Inventory, error) {
	inv, err := routes.LoadInventory(p.Path("routes.json"))
	if err != nil {
		return har.Options{}, nil, configErr("%v", err)
	}
	for i := range inv.Routes {
		inv.Routes[i].Target = p.Canonical(inv.Routes[i].Target)
	}
	o := har.Options{Routes: inv, Target: target, Personal: true}
	if len(p.Identities) > 0 || len(p.Tokens.Command) > 0 || p.Tokens.File != "" {
		tokens, err := capture.LoadTokens(ctx, p)
		if err != nil {
			fmt.Fprintf(out, "identities are not recognised (%v); signed-in requests get generate.identity\n", err)
		} else {
			o.Identify = identify(tokens)
		}
	}
	return o, inv, nil
}

// identify recognises a configured identity by its token: the whole
// Authorization value, its credentials after the scheme, or a cookie value.
func identify(tokens map[string]string) func(authorization, cookie string) string {
	names := make([]string, 0, len(tokens))
	for name, t := range tokens {
		if t != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return func(authorization, cookie string) string {
		cred := authorization
		if _, rest, ok := strings.Cut(authorization, " "); ok {
			cred = strings.TrimSpace(rest)
		}
		cookies, _ := http.ParseCookie(cookie)
		for _, name := range names {
			t := tokens[name]
			if authorization != "" && (authorization == t || cred == t) {
				return name
			}
			for _, c := range cookies {
				if c.Value == t {
					return name
				}
			}
		}
		return ""
	}
}

// proposal is a new catalog request made from a candidate.
type proposal struct {
	request catalog.Request
	// cand is the pipeline's candidate: a later response of the same request
	// can still mark it Personal (see settle).
	cand  *har.Candidate
	route string // the route it reaches, "GET /items/{id}"; empty without one
	fresh bool   // no catalog request reached the route before
	// signedIn: the request was sent signed in, and neither its token nor
	// generate.identity names an identity.
	signedIn bool
}

// proposer turns candidates into new catalog requests: stable IDs (the
// prefix and a slug of method and path, as generate and har import make
// them), an identity, private storage where the configuration, an identity
// or personal data in the response says so. A candidate the catalog already
// has (the same key, see har.Builder.Key) gives none.
type proposer struct {
	p       *config.Project
	b       *har.Builder
	inv     *routes.Inventory
	target  string
	prefix  string // "har", "rec"
	source  string // Source.Import
	ids     map[string]bool
	have    map[string]bool
	covered map[string]bool
}

func newProposer(p *config.Project, cat *catalog.Catalog, b *har.Builder, inv *routes.Inventory, target, prefix, source string) *proposer {
	pr := &proposer{p: p, b: b, inv: inv, target: target, prefix: prefix, source: source,
		ids: map[string]bool{}, have: map[string]bool{}, covered: map[string]bool{}}
	for _, r := range cat.Requests {
		pr.ids[r.ID] = true
		pr.have[b.Key(r)] = true
		if k := pr.unit(r); k != "" {
			pr.covered[k] = true
		}
	}
	return pr
}

// unit is the route and method a request checks, "" without a route.
func (pr *proposer) unit(r catalog.Request) string {
	if pr.inv == nil {
		return ""
	}
	route := routes.Match(pr.inv, r)
	if route == nil {
		return ""
	}
	return route.UnitOf(r.Method) + " " + route.Path
}

func (pr *proposer) propose(c *har.Candidate) *proposal {
	r := catalog.Request{Target: pr.target, Method: c.Method, Path: c.Path, Params: c.Params, Headers: c.Headers,
		Body: c.Body, Identity: c.Identity, Multipart: c.Multipart,
		Mutates: catalog.MutatesFor(c.Method)}
	if c.Auth && r.Identity == "" {
		r.Identity = pr.p.Generate.Identity
	}
	key := pr.b.Key(r)
	if pr.have[key] {
		return nil
	}
	pr.have[key] = true
	base := pr.prefix + "-" + generate.Slug(c.Method+" "+c.Path)
	id := base
	for n := 2; pr.ids[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	pr.ids[id] = true
	r.ID, r.Storage, r.Source = id, generate.Storage(pr.p, id), &catalog.Source{Import: pr.source}
	out := &proposal{request: r, cand: c, signedIn: c.Auth && r.Identity == ""}
	out.settle()
	if k := pr.unit(r); k != "" {
		out.route, out.fresh = k, !pr.covered[k]
		pr.covered[k] = true
	}
	return out
}

// settle makes the request private when its candidate has an identity or
// personal data in a response; call it again once every response is in.
func (p *proposal) settle() {
	if p.request.Storage == snapshot.Public && (p.cand.Identity != "" || p.cand.Personal) {
		p.request.Storage = snapshot.Private
	}
}
