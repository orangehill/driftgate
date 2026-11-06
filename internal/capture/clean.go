package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/state"
)

// CleanItem is a pristine copy driftgate clean removes: of the state
// component Name, on Server, with its record in the local store.
type CleanItem struct {
	Name, Driver, Server, Pristine, Record string
}

// CleanPlan lists the pristine copies of the project's database
// components (those of names, when given: each must name one), from the
// configuration only: nothing is connected to or removed.
func CleanPlan(ctx context.Context, p *config.Project, names []string) ([]CleanItem, error) {
	want := map[string]bool{}
	for _, n := range names {
		if p.State[n].Driver == "" {
			return nil, configErr("%s is not a database component under state: in %s; clean removes only the pristine copies the configuration names", n, configPath(p))
		}
		want[n] = true
	}
	var out []CleanItem
	for n, sc := range p.State {
		if sc.Driver == "" || len(want) > 0 && !want[n] {
			continue
		}
		dsn, err := sc.ReadDSN(ctx, p.Root)
		if err != nil {
			return nil, dsnErr(sc.DSN, n, err)
		}
		server, err := serverOf(p, sc, dsn)
		if err != nil {
			return nil, configErr("state %s: %v", n, err)
		}
		record, err := pristinePath(p, n, server)
		if err != nil {
			return nil, err
		}
		it := CleanItem{Name: n, Driver: sc.Driver, Server: server, Pristine: sc.Pristine, Record: record}
		if sc.Driver == config.SQLite {
			if _, it.Pristine, err = sqliteFiles(p, sc, server, record); err != nil {
				return nil, configErr("state %s: %v", n, err)
			}
			it.Server = ""
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Clean removes the pristine copies of items (CleanPlan) and their records:
// each copy only when driftgate seed made it (state.Dropper), never the
// working database. A copy that is refused stops the clean and keeps its
// record.
func Clean(ctx context.Context, p *config.Project, items []CleanItem, out, log io.Writer) error {
	only := map[string]bool{}
	for _, it := range items {
		only[it.Name] = true
	}
	// Only the components cleaned are opened: the state is narrowed for
	// the call.
	all := p.State
	p.State = map[string]config.Component{}
	for n, sc := range all {
		if only[n] {
			p.State[n] = sc
		}
	}
	comps, _, err := openServers(ctx, p, nil, log, false)
	p.State = all
	if err != nil {
		return err
	}
	defer func() {
		for _, c := range comps {
			c.Close()
		}
	}()
	byName := map[string]CleanItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	for _, c := range comps {
		it := byName[c.Name()]
		d, ok := c.(state.Dropper)
		if !ok {
			return configErr("state %s: driver %s cannot remove its pristine copy", it.Name, it.Driver)
		}
		was, err := d.DropPristine(ctx)
		if err != nil {
			return envErr("%v", err)
		}
		if err := os.Remove(it.Record); err != nil && !errors.Is(err, os.ErrNotExist) {
			return envErr("state %s: record %s: %v", it.Name, it.Record, err)
		}
		if was {
			fmt.Fprintf(out, "removed %s\n", it.What())
		} else {
			fmt.Fprintf(out, "%s: no pristine copy (record removed)\n", it.What())
		}
	}
	return nil
}

// What names the copy: "state db: database shop_pristine (mysql, 127.0.0.1:3306)".
func (it CleanItem) What() string {
	if it.Driver == config.SQLite {
		return fmt.Sprintf("state %s: file %s (sqlite)", it.Name, it.Pristine)
	}
	return fmt.Sprintf("state %s: database %s (%s, %s)", it.Name, it.Pristine, it.Driver, it.Server)
}
