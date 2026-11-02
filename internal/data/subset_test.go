package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// memSource is a source in memory for the closure: rows as text, nil for
// NULL, ordered like the databases order them (numbers as numbers).
type memSource struct {
	rows  map[string][][][]byte
	where map[string]func(t *Table, row [][]byte) bool
}

func (m *memSource) count(_ context.Context, t *Table) (int64, error) {
	return int64(len(m.rows[t.Name])), nil
}

func (m *memSource) scan(_ context.Context, q scanQuery, fn func(row [][]byte) error) error {
	t := q.t
	at := func(name string) int {
		for i, c := range t.Columns {
			if c.Name == name {
				return i
			}
		}
		panic("no column " + name)
	}
	var rows [][][]byte
	for _, r := range m.rows[t.Name] {
		if q.where != "" && !m.where[q.where](t, r) {
			continue
		}
		if len(q.in) > 0 {
			found := false
			for _, v := range q.vals {
				eq := true
				for j, c := range q.in {
					if r[at(c.Name)] == nil || !bytes.Equal(r[at(c.Name)], v[j]) {
						eq = false
					}
				}
				found = found || eq
			}
			if !found {
				continue
			}
		}
		ok := true
		for _, w := range q.eq {
			if v := r[at(w.col)]; v == nil || string(v) != w.value {
				ok = false
			}
		}
		if ok {
			rows = append(rows, r)
		}
	}
	if q.order != 0 {
		var id []*Column
		for _, k := range t.PrimaryKey {
			id = append(id, t.Column(k))
		}
		if id == nil {
			id = t.Columns
		}
		sort.SliceStable(rows, func(a, b int) bool {
			for _, c := range id {
				x, y := rows[a][at(c.Name)], rows[b][at(c.Name)]
				var cmp int
				if c.Category == CatNumeric {
					nx, _ := strconv.ParseFloat(string(x), 64)
					ny, _ := strconv.ParseFloat(string(y), 64)
					cmp = map[bool]int{true: -1, false: 1}[nx < ny]
					if nx == ny {
						cmp = 0
					}
				} else {
					cmp = bytes.Compare(x, y)
				}
				if cmp != 0 {
					return (cmp < 0) == (q.order > 0)
				}
			}
			return false
		})
	}
	if q.limit > 0 && len(rows) > q.limit {
		rows = rows[:q.limit]
	}
	for _, r := range rows {
		out := make([][]byte, len(q.cols))
		for i, c := range q.cols {
			out[i] = r[at(c.Name)]
		}
		if err := fn(out); err == errStop {
			return nil
		} else if err != nil {
			return err
		}
	}
	return nil
}

// table builds a test table: columns "name" (a number), "name?" (nullable),
// "$name" (text); the first column is the primary key unless pk is false.
func table(name string, pk bool, cols ...string) *Table {
	t := &Table{Name: name, quoted: "`" + name + "`"}
	for _, spec := range cols {
		c := &Column{Category: CatNumeric}
		if strings.HasPrefix(spec, "$") {
			spec, c.Category = spec[1:], CatText
		}
		if strings.HasSuffix(spec, "?") {
			spec, c.Nullable = spec[:len(spec)-1], true
		}
		c.Name = spec
		t.Columns = append(t.Columns, c)
	}
	if pk {
		t.PrimaryKey = []string{t.Columns[0].Name}
	}
	return t
}

func fk(t *Table, col, parent string) {
	t.ForeignKeys = append(t.ForeignKeys, ForeignKey{Name: t.Name + "_" + col, Table: t.Name, Columns: []string{col}, RefTable: parent, RefColumns: []string{"id"}})
}

// rows parses "1,NULL,x; 2,1,y" into rows.
func rows(s string) [][][]byte {
	var out [][][]byte
	for _, r := range strings.Split(s, ";") {
		var row [][]byte
		for _, v := range strings.Split(strings.TrimSpace(r), ",") {
			if v = strings.TrimSpace(v); v == "NULL" {
				row = append(row, nil)
			} else {
				row = append(row, []byte(v))
			}
		}
		out = append(out, row)
	}
	return out
}

func seq(n int, f func(i int) string) string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, f(i))
	}
	return strings.Join(out, "; ")
}

// sampled is the outcome of a subset: per table the kept rows after Keep (with
// nulled references), in source order.
type sampled struct {
	s    *Subset
	kept map[string][][][]byte
}

func (o sampled) ids(table string) string {
	var out []string
	for _, r := range o.kept[table] {
		out = append(out, string(r[0]))
	}
	return strings.Join(out, ",")
}

func runSubset(t *testing.T, schema *Schema, src *memSource, subset string) (sampled, error) {
	t.Helper()
	rules, err := ParseRules([]byte("version: 1\ntables: {}\n" + subset))
	if err != nil {
		t.Fatal(err)
	}
	s := &Subset{dl: mysqlDialect{}}
	if err := s.run(context.Background(), src, schema, rules, io.Discard); err != nil {
		return sampled{}, err
	}
	out := sampled{s: s, kept: map[string][][][]byte{}}
	for _, tb := range schema.Tables {
		for _, r := range src.rows[tb.Name] {
			row := append([][]byte{}, r...)
			if s.Keep(tb, tb.Columns, row) {
				out.kept[tb.Name] = append(out.kept[tb.Name], row)
			}
		}
	}
	closed(t, out)
	return out, nil
}

// closed checks that every reference of every kept row points at a kept row.
func closed(t *testing.T, o sampled) {
	t.Helper()
	for _, st := range o.s.order {
		for _, row := range o.kept[st.t.Name] {
			for _, r := range st.out {
				v := pick(row, r.copyAt)
				if hasNull(v) || !r.matches(row, true) {
					continue
				}
				if _, ok := r.keys.kept[tupleKey(v)]; !ok && r.parent.total > 0 {
					found := false
					for _, p := range o.kept[r.parent.t.Name] {
						found = found || tupleKey(pick(p, positionsOf(r.parent.t, r.refCols))) == tupleKey(v)
					}
					if !found && st.dangling == 0 {
						t.Errorf("%s row %s: %s points outside the sample", st.t.Name, row[0], r)
					}
				}
			}
		}
	}
}

func positionsOf(t *Table, cols []string) []int {
	var out []int
	for _, cn := range cols {
		for i, c := range t.Columns {
			if c.Name == cn {
				out = append(out, i)
			}
		}
	}
	return out
}

func TestSubsetChain(t *testing.T) {
	a, b, c, d := table("a", true, "id"), table("b", true, "id", "a_id"), table("c", true, "id", "b_id"), table("d", true, "id")
	fk(b, "a_id", "a")
	fk(c, "b_id", "b")
	src := &memSource{rows: map[string][][][]byte{
		"a": rows(seq(5, func(i int) string { return fmt.Sprint(i) })),
		"b": rows(seq(5, func(i int) string { return fmt.Sprintf("%d,%d", i, i) })),
		"c": rows(seq(10, func(i int) string { return fmt.Sprintf("%d,%d", i, (i+1)/2) })),
		"d": rows("1; 2"),
	}}
	o, err := runSubset(t, &Schema{Tables: []*Table{a, b, c, d}}, src, "subset:\n  tables:\n    c: { rows: 3 }\n")
	if err != nil {
		t.Fatal(err)
	}
	for tn, want := range map[string]string{"c": "8,9,10", "b": "4,5", "a": "4,5", "d": ""} {
		if got := o.ids(tn); got != want {
			t.Errorf("%s: %q, want %q", tn, got, want)
		}
	}
	if r := o.s.report(c); r.Roots != 3 || r.Total != 10 || r.Parents != 0 {
		t.Errorf("c: %+v", r)
	}
	if r := o.s.report(a); r.Parents != 2 || r.Roots != 0 {
		t.Errorf("a: %+v", r)
	}
	// order: first takes the lowest keys.
	o, _ = runSubset(t, &Schema{Tables: []*Table{a, b, c, d}}, src, "subset:\n  tables:\n    c: { rows: 3, order: first }\n    d: all\n")
	if got := o.ids("c") + "|" + o.ids("a") + "|" + o.ids("d"); got != "1,2,3|1,2|1,2" {
		t.Errorf("first: %s", got)
	}
}

func cycleSchema() (*Schema, *memSource) {
	users := table("users", true, "id", "manager_id?", "team_id?")
	teams := table("teams", true, "id", "lead_id")
	fk(users, "manager_id", "users")
	fk(users, "team_id", "teams")
	fk(teams, "lead_id", "users")
	src := &memSource{rows: map[string][][][]byte{
		// A chain of managers 6 → 5 → … → 1; user 6 is in team 1, led by 2.
		"users": rows("1,NULL,NULL; 2,1,NULL; 3,2,NULL; 4,3,NULL; 5,4,NULL; 6,5,1"),
		"teams": rows("1,2; 2,3"),
	}}
	return &Schema{Tables: []*Table{teams, users}}, src
}

func TestSubsetCycleAndSelfReference(t *testing.T) {
	schema, src := cycleSchema()
	o, err := runSubset(t, schema, src, "subset:\n  tables:\n    users: { rows: 1 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ids("users") + "|" + o.ids("teams"); got != "1,2,3,4,5,6|1" {
		t.Errorf("pulled: %s", got)
	}
	// With nullable null everywhere the manager and the team stay out and
	// both references are nulled.
	o, err = runSubset(t, schema, src, "subset:\n  nullable: null\n  tables:\n    users: { rows: 1 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ids("users") + "|" + o.ids("teams"); got != "6|" || o.kept["users"][0][1] != nil || o.kept["users"][0][2] != nil {
		t.Errorf("nulled: %s", got)
	}
	if r := o.s.report(schema.Tables[1]); r.Nulled != 2 || r.Roots != 1 || r.Parents != 0 {
		t.Errorf("report %+v", r)
	}
	// Only the manager is nulled; the team is pulled in, and its lead (NOT
	// NULL) with it.
	o, err = runSubset(t, schema, src, "subset:\n  tables:\n    users: { rows: 1 }\n  references:\n    - { from: \"users(manager_id)\", nullable: null }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ids("users") + "|" + o.ids("teams"); got != "2,6|1" {
		t.Errorf("per reference: %s", got)
	}
	var managers []string
	for _, r := range o.kept["users"] {
		managers = append(managers, fmt.Sprintf("%s:%v:%v", r[0], r[1] == nil, r[2] == nil))
	}
	if got := strings.Join(managers, " "); got != "2:true:true 6:true:false" {
		t.Errorf("references: %s", got)
	}
	if r := o.s.report(schema.Tables[1]); r.Nulled != 2 || r.Roots != 1 || r.Parents != 1 {
		t.Errorf("report %+v", r)
	}
}

func TestSubsetDeclaredMorphReference(t *testing.T) {
	posts, videos := table("posts", true, "id"), table("videos", true, "id")
	comments := table("comments", true, "id", "$commentable_type", "commentable_id", "user_id")
	users := table("users", true, "id")
	src := &memSource{rows: map[string][][][]byte{
		"posts":    rows("1; 2; 3"),
		"videos":   rows("1; 2; 3"),
		"users":    rows("1; 2; 3"),
		"comments": rows(`1,post,1,1; 2,video,1,1; 3,post,2,2; 4,video,3,2; 5,other,9,3; 6,post,99,3`),
	}}
	schema := &Schema{Tables: []*Table{comments, posts, users, videos}}
	o, err := runSubset(t, schema, src, `subset:
  tables:
    comments: { rows: 5 }
  references:
    - { from: "comments(commentable_id)", to: "posts(id)", when: { commentable_type: post } }
    - { from: "comments(commentable_id)", to: "videos(id)", when: { commentable_type: video } }
    - { from: "comments(user_id)", to: "users(id)" }
`)
	if err != nil {
		t.Fatal(err)
	}
	for tn, want := range map[string]string{"comments": "2,3,4,5,6", "posts": "2", "videos": "1,3", "users": "1,2,3"} {
		if got := o.ids(tn); got != want {
			t.Errorf("%s: %q, want %q", tn, got, want)
		}
	}
	if r := o.s.report(comments); r.Dangling != 1 {
		t.Errorf("dangling: %+v", r)
	}
}

func followSchema() (*Schema, *memSource) {
	users := table("users", true, "id")
	orders := table("orders", true, "id", "user_id")
	items := table("items", true, "id", "order_id")
	fk(orders, "user_id", "users")
	fk(items, "order_id", "orders")
	src := &memSource{rows: map[string][][][]byte{
		"users":  rows("1; 2; 3; 4"),
		"orders": rows(seq(8, func(i int) string { return fmt.Sprintf("%d,%d", i, (i+1)/2) })),
		"items":  rows(seq(16, func(i int) string { return fmt.Sprintf("%d,%d", i, (i+1)/2) })),
	}}
	return &Schema{Tables: []*Table{items, orders, users}}, src
}

func TestSubsetFollowChildren(t *testing.T) {
	schema, src := followSchema()
	o, err := runSubset(t, schema, src, `subset:
  tables:
    users: { rows: 2 }
    orders: { follow: [users], per_parent: 1 }
    items: { follow: [orders], rows: 1, order: first }
`)
	if err != nil {
		t.Fatal(err)
	}
	// Users 3 and 4 with their latest order and its items; item 1 is a root
	// whose order and user are taken in as parents, without their other
	// orders.
	for tn, want := range map[string]string{"users": "1,3,4", "orders": "1,6,8", "items": "1,11,12,15,16"} {
		if got := o.ids(tn); got != want {
			t.Errorf("%s: %q, want %q", tn, got, want)
		}
	}
	if r := o.s.report(schema.Tables[1]); r.Followed != 2 || r.Parents != 1 {
		t.Errorf("orders: %+v", r)
	}
}

func TestSubsetNoneTable(t *testing.T) {
	tags := table("tags", true, "id")
	posts := table("posts", true, "id", "tag_id?")
	notes := table("notes", true, "id", "tag_id")
	fk(posts, "tag_id", "tags")
	fk(notes, "tag_id", "tags")
	src := &memSource{rows: map[string][][][]byte{"tags": rows("1; 2"), "posts": rows("1,1; 2,NULL; 3,2"), "notes": rows("1,1")}}
	schema := &Schema{Tables: []*Table{notes, posts, tags}}
	o, err := runSubset(t, schema, src, "subset:\n  tables:\n    tags: none\n    posts: { rows: 2 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ids("posts") + "|" + o.ids("tags"); got != "2,3|" {
		t.Errorf("%s", got)
	}
	if o.kept["posts"][1][1] != nil || o.s.report(posts).Nulled != 1 {
		t.Error("the reference to an empty table is not nulled")
	}
	_, err = runSubset(t, schema, src, "subset:\n  tables:\n    tags: none\n    notes: all\n")
	var ce *CheckError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "notes(tag_id) → tags(id)") {
		t.Fatalf("a NOT NULL reference to a table without rows: %v", err)
	}
}

func TestSubsetPercentIsStable(t *testing.T) {
	items := table("items", true, "id")
	all := rows(seq(2000, func(i int) string { return fmt.Sprint(i) }))
	src := &memSource{rows: map[string][][][]byte{"items": all}}
	schema := &Schema{Tables: []*Table{items}}
	sub := "subset:\n  tables:\n    items: { percent: 10 }\n"
	a, _ := runSubset(t, schema, src, sub)
	n := len(a.kept["items"])
	if n < 150 || n > 250 {
		t.Fatalf("10%% of 2000 is %d rows", n)
	}
	b, _ := runSubset(t, schema, src, sub)
	if a.ids("items") != b.ids("items") {
		t.Fatal("two runs chose different rows")
	}
	// A row's choice does not depend on the other rows.
	src.rows["items"] = all[:1000]
	c, _ := runSubset(t, schema, src, sub)
	if !strings.HasPrefix(a.ids("items"), c.ids("items")) {
		t.Fatal("removing rows changed the choice of the rest")
	}
	// rows caps a percentage, highest keys first.
	src.rows["items"] = all
	d, _ := runSubset(t, schema, src, "subset:\n  tables:\n    items: { percent: 10, rows: 5 }\n")
	if got := len(d.kept["items"]); got != 5 || !strings.HasSuffix(a.ids("items"), d.ids("items")) {
		t.Fatalf("percent with rows: %s", d.ids("items"))
	}
}

func TestSubsetWhereAndNoPrimaryKey(t *testing.T) {
	users := table("users", true, "id", "$status")
	pivot := table("role_user", false, "user_id", "role_id")
	roles := table("roles", true, "id")
	fk(pivot, "user_id", "users")
	fk(pivot, "role_id", "roles")
	src := &memSource{
		rows: map[string][][][]byte{
			"users":     rows("1,active; 2,blocked; 3,active"),
			"roles":     rows("1; 2"),
			"role_user": rows("1,1; 2,2; 3,1; 3,2"),
		},
		where: map[string]func(*Table, [][]byte) bool{
			"status = 'active'": func(_ *Table, r [][]byte) bool { return string(r[1]) == "active" },
		},
	}
	schema := &Schema{Tables: []*Table{roles, pivot, users}}
	o, err := runSubset(t, schema, src, "subset:\n  tables:\n    users: { where: \"status = 'active'\" }\n    role_user: { follow: [users] }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ids("users") + "|" + o.ids("role_user") + "|" + o.ids("roles"); got != "1,3|1,3,3|1,2" {
		t.Errorf("%s", got)
	}
	if w := o.s.Where(roles); w != "" {
		t.Errorf("every row is kept: %q", w)
	}
	if w := o.s.Where(users); w != "`id` IN (1, 3)" {
		t.Errorf("where %q", w)
	}
	if w := o.s.Where(pivot); w != "" {
		t.Errorf("a table without a primary key is read whole: %q", w)
	}
}

// TestSubsetKeyWithoutPrimaryKey: a kept row of a table without a primary
// key is held by a hash of its columns, not by their values (BLOBs too).
func TestSubsetKeyWithoutPrimaryKey(t *testing.T) {
	users := table("users", true, "id")
	files := table("files", false, "user_id", "$body")
	fk(files, "user_id", "users")
	big := strings.Repeat("x", 100000)
	src := &memSource{rows: map[string][][][]byte{
		"users": rows("1; 2"),
		"files": rows("1," + big + "; 1," + big + "y; 2," + big),
	}, where: map[string]func(*Table, [][]byte) bool{
		"id = 1": func(_ *Table, r [][]byte) bool { return string(r[0]) == "1" },
	}}
	o, err := runSubset(t, &Schema{Tables: []*Table{users, files}}, src, "subset:\n  tables:\n    users: { where: \"id = 1\" }\n    files: { follow: [users] }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ids("files"); got != "1,1" {
		t.Errorf("kept files of users %s", got)
	}
	st := o.s.tables["files"]
	if len(st.kept) != 2 {
		t.Fatalf("%d kept keys", len(st.kept))
	}
	for k := range st.kept {
		if len(k) != 16 {
			t.Errorf("a key of %d bytes", len(k))
		}
	}
}

func TestSubsetRulesErrors(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"subset:\n  tables:\n    a: some\n", `"some" is not all, none or a mapping`},
		{"subset:\n  tables:\n    a: { rows: 1, limit: 2 }\n", `unknown key "limit"`},
		{"subset:\n  tables:\n    a: { percent: 120 }\n", "percent must be above 0"},
		{"subset:\n  tables:\n    a: { order: newest, rows: 1 }\n", "order must be latest or first"},
		{"subset:\n  tables:\n    a: { per_parent: 2, rows: 1 }\n", "per_parent needs follow"},
		{"subset:\n  tables:\n    a: {}\n", "needs rows, percent, where or follow"},
		{"subset:\n  nullable: drop\n", "nullable must be pull or null"},
		{"subset:\n  references:\n    - { from: a.b }\n", "is not table(column, …)"},
		{"subset:\n  references:\n    - { from: \"a(x, y)\", to: \"b(id)\" }\n", "another number of columns"},
		{"subset:\n  references:\n    - { from: \"a(x)\", when: { t: x } }\n", "when needs to"},
	} {
		_, err := ParseRules([]byte("version: 1\ntables: {}\n" + c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.yaml, err, c.want)
		}
	}

	schema, src := followSchema()
	for _, c := range []struct{ yaml, want string }{
		{"subset:\n  tables:\n    gone: all\n", "subset table gone"},
		{"subset:\n  tables:\n    users: { follow: [items] }\n", "follows items but has no reference to it"},
		{"subset:\n  references:\n    - { from: \"items(id)\" }\n", "no foreign key on these columns"},
		{"subset:\n  references:\n    - { from: \"items(nope)\", to: \"users(id)\" }\n", "column items.nope"},
		{"subset:\n  references:\n    - { from: \"items(order_id)\", nullable: null }\n", "nullable null on a NOT NULL column"},
	} {
		_, err := runSubset(t, schema, src, c.yaml)
		var ce *CheckError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.yaml, err, c.want)
		}
	}
	if _, err := runSubset(t, schema, src, ""); !errors.Is(err, errNoSubset) {
		t.Errorf("no subset section: %v", err)
	}
}

func TestTupleKey(t *testing.T) {
	for _, v := range [][][]byte{{[]byte("a"), nil, {}}, {nil}, {[]byte(strings.Repeat("x", 300)), []byte("1")}} {
		got := splitKey(tupleKey(v))
		if len(got) != len(v) {
			t.Fatalf("%q → %q", v, got)
		}
		for i := range v {
			if (v[i] == nil) != (got[i] == nil) || !bytes.Equal(v[i], got[i]) {
				t.Fatalf("%q → %q", v, got)
			}
		}
	}
	if tupleKey([][]byte{[]byte("a"), []byte("b")}) == tupleKey([][]byte{[]byte("ab"), {}}) {
		t.Fatal("ambiguous")
	}
}
