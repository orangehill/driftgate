package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/routes"
)

// names is a flag given several times or as a comma-separated list.
type names []string

func (n *names) String() string { return strings.Join(*n, ",") }

func (n *names) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*n = append(*n, s)
		}
	}
	return nil
}

// once is a flag that may be given only once.
type once struct {
	v   string
	set bool
}

func (o *once) String() string { return o.v }

func (o *once) Set(v string) error {
	if o.set {
		return errors.New("given more than once")
	}
	o.v, o.set = v, true
	return nil
}

func targetFlag(fs *flag.FlagSet) *names {
	n := &names{}
	fs.Var(n, "target", "")
	return n
}

// selection is the services a command works on; nil means every service.
type selection map[string]bool

func selectTargets(p *config.Project, n *names) (selection, error) {
	if len(*n) == 0 {
		return nil, nil
	}
	sel := selection{}
	for _, name := range *n {
		if _, ok := p.Services()[name]; !ok || len(p.Targets) == 0 {
			return nil, configErr("--target %s: no such service (services: %s)", name, strings.Join(p.TargetNames(), ", "))
		}
		sel[name] = true
	}
	return sel, nil
}

func (s selection) has(name string) bool { return s == nil || s[name] }

func (s selection) names() []string {
	out := make([]string, 0, len(s))
	for n := range s {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// requests keeps the catalog's requests of the selected services.
func (s selection) requests(cat *catalog.Catalog) *catalog.Catalog {
	if s == nil {
		return cat
	}
	out := &catalog.Catalog{Version: cat.Version, Requests: []catalog.Request{}}
	kept := map[string]bool{}
	for _, r := range cat.Requests {
		if s[r.Target] {
			out.Requests = append(out.Requests, r)
			kept[r.ID] = true
		}
	}
	for _, seq := range cat.Sequences {
		var ids []string
		for _, id := range seq.Requests {
			if kept[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			seq.Requests = ids
			out.Sequences = append(out.Sequences, seq)
		}
	}
	return out
}

// others keeps what requests drops: the catalog of the other services.
func (s selection) others(cat *catalog.Catalog) *catalog.Catalog {
	rest := selection{}
	for _, r := range cat.Requests {
		if !s[r.Target] {
			rest[r.Target] = true
		}
	}
	return rest.requests(cat)
}

// routes keeps the routes of the selected services.
func (s selection) routes(inv *routes.Inventory) *routes.Inventory {
	if s == nil {
		return inv
	}
	out := *inv
	out.Routes = nil
	for _, r := range inv.Routes {
		if s[r.Target] {
			out.Routes = append(out.Routes, r)
		}
	}
	return &out
}

// partial says that only some services were checked.
func (s selection) partial(p *config.Project, out io.Writer) {
	if s == nil {
		return
	}
	var skipped []string
	for _, n := range p.TargetNames() {
		if !s[n] {
			skipped = append(skipped, n)
		}
	}
	fmt.Fprintf(out, "\nPARTIAL CHECK — checked: %s; not checked: %s. Without --target, run checks every service.\n",
		strings.Join(s.names(), ", "), strings.Join(skipped, ", "))
}
