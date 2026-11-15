package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/state"
)

// CleanItem is a pristine copy driftgate clean removes: of the state
// component Name, on Server, with its record in the local store.
type CleanItem struct {
	Name, Driver, Server, Pristine, Record string
	Working                                string // SQLite: the working file's absolute path
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
		if sc.Driver == config.SQLite {
			it, err := sqliteCleanItem(p, n, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
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
		out = append(out, CleanItem{Name: n, Driver: sc.Driver, Server: server, Pristine: sc.Pristine, Record: record})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// sqliteCleanItem is the CleanItem of an SQLite component. Its working
// file need not exist (a worktree git did not give it): the record is
// found by the path the file had, which is how serverOf names it. When
// neither that record nor a pristine file named in the configuration is
// there, nothing can be found safely, and it says so.
func sqliteCleanItem(p *config.Project, n string, sc config.Component) (CleanItem, error) {
	working, err := appconf.SQLitePath(p.Root, sc.Working)
	if err != nil {
		return CleanItem{}, configErr("state %s: working: %v", n, err)
	}
	server := sqliteServerPrefix + working
	record, err := pristinePath(p, n, server)
	if err != nil {
		return CleanItem{}, err
	}
	it := CleanItem{Name: n, Driver: sc.Driver, Record: record, Working: working}
	if _, it.Pristine, err = sqliteFiles(p, sc, server, record); err != nil {
		return CleanItem{}, configErr("state %s: %v", n, err)
	}
	if _, err := os.Stat(working); errors.Is(err, os.ErrNotExist) && sc.Pristine == "" {
		if _, err := os.Stat(record); errors.Is(err, os.ErrNotExist) {
			return CleanItem{}, configErr("state %s: the working file %s is not there, and no record of a pristine copy for it is in the local store, so clean cannot tell which copy is its own; make the working file again, or remove its copy in %s yourself", n, sc.Working, filepath.Dir(record))
		}
	}
	return it, nil
}

// Clean removes the pristine copies of items (CleanPlan) and their records:
// each copy only when driftgate seed made it (state.Dropper), never the
// working database. A copy that is refused stops the clean and keeps its
// record.
func Clean(ctx context.Context, p *config.Project, items []CleanItem, out, log io.Writer) error {
	only := map[string]bool{}
	for _, it := range items {
		// SQLite's copy is removed without opening the working file.
		if it.Driver != config.SQLite {
			only[it.Name] = true
		}
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
	droppers := map[string]state.Dropper{}
	for _, c := range comps {
		d, ok := c.(state.Dropper)
		if !ok {
			return configErr("state %s: driver %s cannot remove its pristine copy", c.Name(), byName[c.Name()].Driver)
		}
		droppers[c.Name()] = d
	}
	for _, it := range items {
		var was bool
		var err error
		if it.Driver == config.SQLite {
			was, err = state.DropSQLitePristine(ctx, state.Options{Name: it.Name, Working: it.Working, Pristine: it.Pristine})
		} else {
			was, err = droppers[it.Name].DropPristine(ctx)
		}
		if err != nil {
			return envErr("%v", err)
		}
		if err := removeRecords(it.Record); err != nil {
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
