package capture

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// A capture starts with a restore, which brings the working database back
// to the pristine copy. When the schema was changed since the copy was
// made (a migration that ran on the working database), that restore undoes
// it silently, and the capture runs the application on the old schema: a
// pass that proves nothing about the new one. Before the first restore,
// the working database is compared with the pristine copy, from the
// fingerprint that restore takes anyway:
//
//   - an item on one side only (a table, view, trigger … made or dropped);
//   - an item that holds no rows (a view, trigger, function, pragma) whose
//     hash differs: its definition changed;
//   - a table that differs whose definition differs too (state.Definer):
//     its columns, keys or indexes changed, not only its rows;
//   - the table a migration tool records the migrations it ran in
//     (migrationTables) whose rows differ: a migration ran, which may also
//     have changed data only.
//
// Rows the application wrote (its own requests, a user trying it out) are
// none of these: they never stop a capture. A sequence's value moves with
// the rows, so only a sequence made or dropped counts.
//
// Without prepare: the capture stops before the restore, so the working
// database stays as it is. With prepare: the restore and prepare run, and
// each of those items must then differ from the pristine copy again: the
// migrations prepare runs made it again. Else the capture stops.

// outsideChange is an item of the working database changed outside
// DriftGate that a restore undoes; how says in what way.
type outsideChange struct {
	component, item, how string
}

const (
	outsideNew        = "made since the pristine copy"
	outsideGone       = "dropped since the pristine copy"
	outsideDefinition = "its definition changed"
	outsideMigrations = "migrations ran"
)

// migrationTables are the tables migration tools record the migrations
// they ran in (lower case; a PostgreSQL or SQL Server item names its
// schema first): Laravel, Rails, Symfony (Doctrine), Django, ASP.NET Core
// (Entity Framework), Spring Boot (Flyway, Liquibase), Prisma, Drizzle,
// Knex, Sequelize, TypeORM, MikroORM, Kysely, Alembic, Phinx, goose,
// golang-migrate, sqlx and Diesel.
var migrationTables = map[string]bool{
	"migrations": true, "schema_migrations": true, "doctrine_migration_versions": true, "migration_versions": true,
	"django_migrations": true, "__efmigrationshistory": true, "flyway_schema_history": true, "databasechangelog": true,
	"_prisma_migrations": true, "__drizzle_migrations": true, "knex_migrations": true, "sequelizemeta": true,
	"mikro_orm_migrations": true, "kysely_migration": true, "alembic_version": true, "phinxlog": true,
	"goose_db_version": true, "_sqlx_migrations": true, "__diesel_schema_migrations": true,
}

// frameworkMigrations is the table the migration tool of a framework
// discover: names records its migrations in.
var frameworkMigrations = map[string]string{"laravel": "migrations", "rails": "schema_migrations", "django": "django_migrations", "symfony": "doctrine_migration_versions"}

// migrationNames are the migrations tables of a project: those of the
// frameworks its discover: names, or every one migrationTables knows when
// it names none of them. An application's own table named migrations is
// then no migrations table in a Rails project.
func migrationNames(p *config.Project) map[string]bool {
	names := map[string]bool{}
	for _, a := range p.Discover {
		if n, ok := frameworkMigrations[a.Adapter]; ok {
			names[n] = true
		}
	}
	if len(names) == 0 {
		return migrationTables
	}
	return names
}

// migrationTable reports whether item is one of the project's migrations
// tables (migrationNames; every one known without a project).
func (t *tracker) migrationTable(item string) bool {
	if t.migrations == nil {
		return migrationTable(item)
	}
	name, ok := strings.CutPrefix(item, "table:")
	if !ok {
		return false
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return t.migrations[strings.ToLower(name)]
}

// migrationTable reports whether item is the table:<name> of a table
// migrationTables names, in any schema and letter case.
func migrationTable(item string) bool {
	name, ok := strings.CutPrefix(item, "table:")
	if !ok {
		return false
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return migrationTables[strings.ToLower(name)]
}

// rowKinds are the kinds of items whose hash includes rows or a value that
// moves with them; any other item holds a definition only.
var rowKinds = map[string]bool{"table": true, "matview": true, "sequence": true}

// survey fingerprints every database component before the first restore,
// keeps the fingerprint for that restore, and returns the items changed
// outside DriftGate that the restore would undo (see above). What it read
// of the pristine copy's changed tables is kept for redone.
func (t *tracker) survey(ctx context.Context) ([]outsideChange, error) {
	var out []outsideChange
	t.found, t.foundRO = states{}, states{}
	t.outsideDefs, t.outsideRows = states{}, states{}
	t.outsideRO = map[string]map[string]bool{}
	for _, c := range t.comps {
		pf, isDB := t.pristine[c.Name()]
		if !isDB {
			continue
		}
		// The restore's own fingerprint, taken early: its time is the
		// restore's.
		start := time.Now()
		cur, err := c.Fingerprint(ctx, nil)
		t.time.Restore += time.Since(start)
		if err != nil {
			return nil, envErr("state %s: fingerprint: %v", c.Name(), err)
		}
		t.found[c.Name()] = cur
		// A record of an older fingerprint format hashes views and
		// triggers another way: their hashes are not compared (the restore
		// asks for a seed when they differ); tables are compared by their
		// definitions, read anew on both sides.
		older := t.formats[c.Name()] != state.FingerprintFormat
		var tables, migrations, unknown []string
		d, definer := c.(state.Definer)
		for _, it := range state.Diff(cur, pf) {
			_, now := cur[it]
			_, then := pf[it]
			kind, _, _ := strings.Cut(it, ":")
			switch {
			case older && now != then && kind != "table":
				// The older format may not make such an item at all
				// (PostgreSQL's functions, types and schemas came with
				// format 2): only a seed tells.
				unknown = append(unknown, it)
			case !then:
				out = append(out, outsideChange{c.Name(), it, outsideNew})
			case !now:
				out = append(out, outsideChange{c.Name(), it, outsideGone})
			case !rowKinds[kind]:
				if !older {
					out = append(out, outsideChange{c.Name(), it, outsideDefinition})
				}
			case t.migrationTable(it) && !definer:
				out = append(out, outsideChange{c.Name(), it, outsideMigrations})
			case definer:
				tables = append(tables, it)
			}
		}
		if len(unknown) > 0 {
			return nil, envErr("state %s: re-seed needed: the record of the pristine copy has an older fingerprint format, which may not make these items at all: %v; run driftgate seed, then, if a capture says the baseline's state differs, driftgate verify --baseline <name> --migrate-state", c.Name(), limit(unknown, 20))
		}
		// The restore_only and restore_empty tables are restored at every
		// restore without being fingerprinted: their definitions are
		// compared every time (their rows never are).
		restoreOnly := map[string]bool{}
		if want, ok := t.ro[c.Name()]; ok && definer {
			for it := range want {
				if kind, _, _ := strings.Cut(it, ":"); kind == "table" {
					tables = append(tables, it)
					restoreOnly[it] = true
				}
			}
			// One made since the seed: from the fingerprint of the
			// restore_only tables the first restore reads anyway (kept
			// for it).
			start := time.Now()
			now, err := c.(state.RestoreOnly).RestoreOnlyFingerprint(ctx, false)
			t.time.Restore += time.Since(start)
			if err != nil {
				return nil, envErr("state %s: fingerprint of the restore_only tables: %v", c.Name(), err)
			}
			t.foundRO[c.Name()] = now
			for it := range now {
				if kind, _, _ := strings.Cut(it, ":"); kind == "table" && !restoreOnly[it] {
					out = append(out, outsideChange{c.Name(), it, outsideNew})
					restoreOnly[it] = true
				}
			}
		}
		t.outsideRO[c.Name()] = restoreOnly
		if len(tables) == 0 {
			continue
		}
		start = time.Now()
		work, pris, err := definitions(ctx, d, tables, t.pristineDefs[c.Name()])
		if err != nil {
			return nil, envErr("state %s: %v", c.Name(), err)
		}
		t.outsideDefs[c.Name()] = pris
		changed := map[string]bool{}
		for _, it := range state.Diff(work, pris) {
			how := outsideDefinition
			if _, ok := work[it]; !ok {
				how = outsideGone
			}
			out = append(out, outsideChange{c.Name(), it, how})
			changed[it] = true
		}
		for _, it := range tables {
			if t.migrationTable(it) && !changed[it] && !restoreOnly[it] {
				migrations = append(migrations, it)
			}
		}
		if len(migrations) > 0 {
			work, pris, err := contents(ctx, d, migrations)
			if err != nil {
				return nil, envErr("state %s: %v", c.Name(), err)
			}
			t.outsideRows[c.Name()] = pris
			for _, it := range state.Diff(work, pris) {
				out = append(out, outsideChange{c.Name(), it, outsideMigrations})
			}
		}
		t.time.Fingerprint += time.Since(start)
	}
	sortChanges(out)
	return out, nil
}

// definitions reads the definitions of tables in the working database, and
// takes the pristine copy's from recorded, the record of the seed, which
// has every table of the copy; a record of an earlier release has none, and
// they are read from the copy.
func definitions(ctx context.Context, d state.Definer, tables []string, recorded state.Fingerprint) (work, pris state.Fingerprint, err error) {
	if work, err = d.Definitions(ctx, tables, false); err != nil {
		return nil, nil, fmt.Errorf("definitions: %v", err)
	}
	if recorded != nil && len(recorded.Subset(tables)) == len(tables) {
		return work, recorded.Subset(tables), nil
	}
	if pris, err = d.Definitions(ctx, tables, true); err != nil {
		return nil, nil, fmt.Errorf("definitions of the pristine copy: %v", err)
	}
	return work, pris, nil
}

// contents reads the rows of tables in the working database and in the
// pristine copy.
func contents(ctx context.Context, d state.Definer, tables []string) (work, pris state.Fingerprint, err error) {
	if work, err = d.Contents(ctx, tables, false); err != nil {
		return nil, nil, fmt.Errorf("rows: %v", err)
	}
	if pris, err = d.Contents(ctx, tables, true); err != nil {
		return nil, nil, fmt.Errorf("rows of the pristine copy: %v", err)
	}
	return work, pris, nil
}

func sortChanges(list []outsideChange) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].component != list[j].component {
			return list[i].component < list[j].component
		}
		return list[i].item < list[j].item
	})
}

// redone returns the changes prepare did not make again: after the
// restore and prepare, each item must differ from the pristine copy again,
// a table by what survey found changed (its definition, or the rows of a
// migrations table). after is a fingerprint of the working state taken
// after prepare.
func (t *tracker) redone(ctx context.Context, changes []outsideChange, after states) ([]outsideChange, error) {
	var left []outsideChange
	defs, rows := map[string][]outsideChange{}, map[string][]outsideChange{}
	for _, ch := range changes {
		pf := t.pristine[ch.component]
		now, inNow := after[ch.component][ch.item]
		then, inThen := pf[ch.item]
		kind, _, _ := strings.Cut(ch.item, ":")
		_, definer := t.outsideDefs[ch.component]
		switch {
		case ch.how == outsideDefinition && rowKinds[kind], t.outsideRO[ch.component][ch.item]:
			// A restore_only table, made, dropped or changed, is in no
			// fingerprint: its definition (none when it is gone) tells.
			defs[ch.component] = append(defs[ch.component], ch)
		case ch.how == outsideMigrations && definer:
			rows[ch.component] = append(rows[ch.component], ch)
		case inNow == inThen && now == then:
			left = append(left, ch)
		}
	}
	for _, c := range t.comps {
		d, ok := c.(state.Definer)
		if !ok {
			continue
		}
		start := time.Now()
		for _, x := range []struct {
			changes []outsideChange
			read    func(context.Context, []string, bool) (state.Fingerprint, error)
			pris    state.Fingerprint
		}{
			{defs[c.Name()], d.Definitions, t.outsideDefs[c.Name()]},
			{rows[c.Name()], d.Contents, t.outsideRows[c.Name()]},
		} {
			if len(x.changes) == 0 {
				continue
			}
			items := make([]string, len(x.changes))
			for i, ch := range x.changes {
				items[i] = ch.item
			}
			work, err := x.read(ctx, items, false)
			if err != nil {
				return nil, envErr("state %s: %v", c.Name(), err)
			}
			// The same as the pristine copy (both absent for a table made
			// outside and not made again): prepare did not make it.
			for _, ch := range x.changes {
				if work[ch.item] == x.pris[ch.item] {
					left = append(left, ch)
				}
			}
		}
		t.time.Fingerprint += time.Since(start)
	}
	sortChanges(left)
	return left, nil
}

// changedItems lists the changes as <component>/<item> (how), at most n.
func changedItems(changes []outsideChange, n int) string {
	var list []string
	for _, ch := range changes {
		list = append(list, ch.component+"/"+ch.item+" ("+ch.how+")")
	}
	return strings.Join(limit(list, n), ", ")
}

// migrateCommand is the command that runs the migrations of the
// framework discover: names, as prepare: takes it, or "" when there is no
// one such framework.
func migrateCommand(p *config.Project) string {
	cmd := migrateArgs(p)
	if cmd == nil {
		return ""
	}
	quoted := make([]string, len(cmd))
	for i, w := range cmd {
		quoted[i] = fmt.Sprintf("%q", w)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// migrateArgs is migrateCommand as a program and its arguments, or nil.
func migrateArgs(p *config.Project) []string {
	var cmd []string
	for _, a := range p.Discover {
		var c []string
		switch a.Adapter {
		case "laravel":
			php := a.PHP
			if len(php) == 0 {
				php = []string{"php"}
			}
			c = append(append([]string{}, php...), "artisan", "migrate", "--force")
		case "rails":
			c = []string{"bin/rails", "db:migrate"}
		case "django":
			c = []string{"python", "manage.py", "migrate", "--noinput"}
		case "symfony":
			c = []string{"php", "bin/console", "doctrine:migrations:migrate", "--no-interaction", "--allow-no-migration"}
		default:
			continue
		}
		if cmd != nil && strings.Join(cmd, "\x00") != strings.Join(c, "\x00") {
			return nil
		}
		cmd = c
	}
	return cmd
}

// outsideErr is the error of a capture that stops for changes made outside
// DriftGate; restored says whether the restore and prepare ran already
// (prepare did not make them again).
func outsideErr(p *config.Project, changes []outsideChange, opt Options, restored bool) error {
	prepare := "the command that runs the migrations to prepare:"
	if cmd := migrateCommand(p); cmd != "" {
		prepare = "prepare: [" + cmd + "]"
	}
	what := "the working database was changed outside DriftGate since the pristine copy was made: " + changedItems(changes, 20)
	var mine []string
	for _, ch := range changes {
		if ch.how == outsideMigrations {
			mine = append(mine, ch.component+"/"+ch.item)
		}
	}
	hint := ""
	if len(mine) > 0 {
		// A table of the application that happens to have the name.
		hint = fmt.Sprintf(". When %s is a table of the application, not of a migration tool, exclude it or rename it", strings.Join(mine, ", "))
	}
	seed := "run driftgate seed to make the pristine copy anew (--skip-commands keeps the data as it is now), then " + rebaseSteps(p, opt.Baseline)
	migrates := false
	if args := migrateArgs(p); args != nil {
		for _, c := range p.Prepare {
			migrates = migrates || slices.Equal(c.Run, args)
		}
	}
	switch {
	case restored && migrates:
		return envErr("%s. The restore at the start of the capture undid that, and prepare: runs the migrations already, so no migration of this checkout makes it: it comes from another branch, or was made by hand. The capture would run without it and could pass. Fix: if it belongs to this checkout, add its migration; otherwise %s%s", what, seed, hint)
	case restored:
		return envErr("%s. The restore at the start of the capture undid that, and prepare did not make it again, so the capture would run on the old schema and could pass. Fix: add %s in %s; or run the migrations again (the restore left the pristine copy's data) and %s%s", what, prepare, configPath(p), seed, hint)
	case opt.ReadOnly:
		return envErr("%s. The restore at the start of a capture would undo that, and the capture would run on the old schema and could pass, so nothing was restored. Fix (a --read-only capture takes no prepare:): %s%s", what, seed, hint)
	}
	return envErr("%s. The restore at the start of a capture would undo that, and the capture would run on the old schema and could pass, so nothing was restored. Fix: add %s in %s, so every capture runs the migrations after the restore and the baseline stays valid; or %s%s", what, prepare, configPath(p), seed, hint)
}

// rebaseSteps says how a baseline captured from other data takes the data
// of a new pristine copy: a capture without the check of the baseline's
// state, its comparison, and a rebase once its drifts are right.
func rebaseSteps(p *config.Project, baseline *snapshot.Set) string {
	name := "local"
	if baseline != nil {
		name = filepath.Base(baseline.Dir)
	}
	freeze := ""
	if p.Mode == "freeze" {
		freeze = " --allow-freeze"
	}
	return fmt.Sprintf("make the baseline's state the new one: driftgate capture <tag> --skip-fingerprint-check, driftgate compare <tag> --skip-fingerprint-check, and once its drifts are right, driftgate baselines rebase %s --from <tag>%s --reason \"…\"", name, freeze)
}
