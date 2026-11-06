package data

import (
	"errors"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/pii"
)

const exampleRules = `version: 1
tables:
  users:
    columns:
      id: keep
      name: universal
      email: { kind: personal, strategy: email }
      password: password
      bio: free-text
      settings:
        kind: personal
        strategy: json
        paths: { "$.phone": universal, "$.contacts[*].email": email }
  sessions:
    action: empty
    reason: rebuilt by the application
`

func testSchema() *Schema {
	return &Schema{Driver: MySQL, Tables: []*Table{
		{Name: "users", PrimaryKey: []string{"id"}, Columns: []*Column{
			{Name: "id", Type: "int", Category: CatNumeric},
			{Name: "name", Type: "varchar(100)", Category: CatText},
			{Name: "email", Type: "varchar(191)", Category: CatText, Unique: true},
			{Name: "password", Type: "varchar(255)", Category: CatText},
			{Name: "bio", Type: "text", Category: CatText, Nullable: true},
			{Name: "settings", Type: "json", Category: CatJSON, Nullable: true},
		}},
		{Name: "sessions", Columns: []*Column{{Name: "id", Category: CatText}, {Name: "payload", Category: CatText}}},
	}}
}

func TestRulesParseAndCheck(t *testing.T) {
	r, err := ParseRules([]byte(exampleRules))
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Tables["users"].Columns["name"]; c.Kind != KindPersonal || c.Strategy != StrategyUniversal {
		t.Fatalf("short form: %+v", c)
	}
	if err := r.Check(testSchema(), false); err != nil {
		t.Fatal(err)
	}
}

func TestUnclassifiedColumnFails(t *testing.T) {
	r, _ := ParseRules([]byte(exampleRules))
	s := testSchema()
	s.Tables[0].Columns = append(s.Tables[0].Columns, &Column{Name: "phone", Type: "varchar(20)", Category: CatText})
	s.Tables = append(s.Tables, &Table{Name: "orders", Columns: []*Column{{Name: "id", Category: CatNumeric}}})
	err := r.Check(s, false)
	var ce *CheckError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v", err)
	}
	if strings.Join(ce.Unclassified, ",") != "users.phone,orders.id" {
		t.Fatalf("unclassified %v", ce.Unclassified)
	}
	if !strings.Contains(err.Error(), "users.phone") {
		t.Fatalf("message %q", err)
	}
}

func TestCheckFindsMisfits(t *testing.T) {
	r, _ := ParseRules([]byte(exampleRules))
	r.Tables["users"].Columns["id"] = &ColumnRule{Kind: KindPersonal, Strategy: StrategyNull} // NOT NULL
	r.Tables["users"].Columns["bio"] = &ColumnRule{Kind: KindFile}                            // no placeholders
	r.Tables["users"].Columns["gone"] = &ColumnRule{Kind: KindKeep}                           // stale
	r.Tables["users"].Columns["name"] = &ColumnRule{Kind: KindPersonal, Strategy: StrategyEmail, Proposed: true}
	err := r.Check(testSchema(), false)
	var ce *CheckError
	if !errors.As(err, &ce) || len(ce.Invalid) != 2 || len(ce.Stale) != 1 || len(ce.Proposed) != 1 {
		t.Fatalf("%v", err)
	}
}

func TestCheckEmptiedParent(t *testing.T) {
	r, _ := ParseRules([]byte(exampleRules))
	s := testSchema()
	s.Tables = append(s.Tables, &Table{Name: "logins", Columns: []*Column{{Name: "session_id", Category: CatText}},
		ForeignKeys: []ForeignKey{{Table: "logins", Columns: []string{"session_id"}, RefTable: "sessions"}}})
	r.Tables["logins"] = &TableRule{Columns: map[string]*ColumnRule{"session_id": {Kind: KindKeep}}}
	var ce *CheckError
	if err := r.Check(s, false); !errors.As(err, &ce) || len(ce.Invalid) != 1 {
		t.Fatalf("%v", err)
	}
}

func TestRulesReject(t *testing.T) {
	for _, bad := range []string{
		"version: 2\ntables: {}\n",
		"version: 1\ntables: {t: {columns: {c: shuffle}}}\n",
		"version: 1\ntables: {t: {columns: {c: {kind: keep, strategy: email}}}}\n",
		"version: 1\ntables: {t: {action: drop}}\n",
		"version: 1\ntables: {t: {columns: {c: {strategy: universal, paths: {$.a: email}}}}}\n",
		"version: 1\ntables: {t: {columns: {c: {strategy: json, paths: {a: email}}}}}\n",
		"version: 1\ntables: {t: {columns: {c: {kind: serialized-php, action: shred}}}}\n",
		"version: 1\ntables: {t: {columns: {c: {kind: serialized-php, action: \"\"}}}}\n",
		"version: 1\ntables:\n  t:\n    columns:\n      c:\n        kind: serialized-php\n        action:\n",
		"version: 1\ntables: {}\nextra: 1\n",
	} {
		if _, err := ParseRules([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMergeRulesKeepsConfirmedRules(t *testing.T) {
	raw := []byte("version: 1\ntables:\n  users:\n    columns:\n      # checked by hand\n      id: keep\n      gone: keep\n")
	c := &Classification{
		Stale: []Ref{{"users", "gone"}},
		Tables: []TableProposal{
			{Table: "users", Columns: []ColumnProposal{{Column: "email", Rule: &ColumnRule{Kind: KindPersonal, Strategy: StrategyEmail, Reason: "proposed: by name", Proposed: true}}}},
			{Table: "audit", New: true, Columns: []ColumnProposal{{Column: "id", Rule: &ColumnRule{Kind: KindKeep, Proposed: true}}}},
			{Table: "cache", New: true, Action: ActionEmpty, Reason: "proposed: cache"},
		},
	}
	out, err := MergeRules(raw, c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# checked by hand", "id: keep", "email: {kind: personal, strategy: email, reason: 'proposed: by name', proposed: true}", "cache:\n    action: empty"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "gone") {
		t.Errorf("stale rule kept:\n%s", s)
	}
	if strings.Index(s, "audit:") > strings.Index(s, "users:") {
		t.Errorf("tables not sorted:\n%s", s)
	}
	r, err := ParseRules(out)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Tables["users"].Columns["email"].Proposed {
		t.Fatal("proposal lost its mark")
	}
	// A fresh file gets the header.
	out, err = MergeRules(nil, c)
	if err != nil || !strings.Contains(string(out), "# Rules of driftgate data") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestOrder(t *testing.T) {
	s := &Schema{Tables: []*Table{
		{Name: "a_comments", ForeignKeys: []ForeignKey{{RefTable: "posts"}, {RefTable: "users"}}},
		{Name: "posts", ForeignKeys: []ForeignKey{{RefTable: "users"}}},
		{Name: "tree", ForeignKeys: []ForeignKey{{RefTable: "tree"}}},
		{Name: "users"},
	}}
	var got []string
	for _, t := range s.Order() {
		got = append(got, t.Name)
	}
	if strings.Join(got, ",") != "users,posts,a_comments,tree" {
		t.Fatalf("%v", got)
	}
	if refs := s.References("users"); len(refs) != 2 {
		t.Fatalf("%v", refs)
	}
}

func referenceSchema() *Schema {
	return &Schema{Driver: MySQL, Tables: []*Table{
		{Name: "orders", PrimaryKey: []string{"id"}, Columns: []*Column{
			{Name: "id", Category: CatNumeric}, {Name: "customer_email", Category: CatText, Nullable: true}, {Name: "owner", Category: CatText}},
			ForeignKeys: []ForeignKey{{Name: "fk", Table: "orders", Columns: []string{"customer_email"}, RefTable: "users", RefColumns: []string{"email"}}}},
		{Name: "users", PrimaryKey: []string{"email"}, Columns: []*Column{{Name: "email", Category: CatText}, {Name: "name", Category: CatText}}},
	}}
}

func TestReferencesNeedTheSameRule(t *testing.T) {
	check := func(rules string) error {
		t.Helper()
		r, err := ParseRules([]byte("version: 1\ntables:\n" + rules))
		if err != nil {
			t.Fatal(err)
		}
		return r.Check(referenceSchema(), false)
	}
	for _, bad := range []string{
		"  orders: {columns: {id: keep, customer_email: universal, owner: keep}}\n  users: {columns: {email: email, name: universal}}\n",
		"  orders: {columns: {id: keep, customer_email: keep, owner: keep}}\n  users: {columns: {email: email, name: universal}}\n",
		// A reference declared for subset: an application without foreign keys.
		"  orders: {columns: {id: keep, customer_email: email, owner: keep}}\n  users: {columns: {email: email, name: universal}}\n" +
			"subset:\n  references:\n    - {from: orders(owner), to: users(name)}\n",
	} {
		err := check(bad)
		var ce *CheckError
		if !errors.As(err, &ce) || len(ce.Invalid) != 1 || !strings.Contains(ce.Invalid[0], "references users.") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for _, good := range []string{
		"  orders: {columns: {id: keep, customer_email: email, owner: keep}}\n  users: {columns: {email: email, name: universal}}\n",
		"  orders: {columns: {id: keep, customer_email: \"null\", owner: keep}}\n  users: {columns: {email: email, name: universal}}\n",
	} {
		if err := check(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
}

func TestRulesFitOtherTypes(t *testing.T) {
	cols := map[string]*Column{
		"cc":       {Name: "cc", Type: "text[]", Category: CatOther, Nullable: true},
		"card":     {Name: "card", Type: "xml", Category: CatOther},
		"search":   {Name: "search", Type: "tsvector", Category: CatOther},
		"status":   {Name: "status", Type: "enum('a','b')", Category: CatOther},
		"ids":      {Name: "ids", Type: "integer[]", Category: CatOther},
		"nick":     {Name: "nick", Type: "text", Category: CatText, Collation: "public.ci", nondeterministic: true},
		"nick_set": {Name: "nick_set", Type: "text", Category: CatText, Collation: "public.ci", nondeterministic: true, Nullable: true},
	}
	for _, c := range []struct {
		col, rule string
		ok        bool
	}{
		{"cc", StrategyEmail, true}, {"cc", StrategyEmpty, true}, {"cc", StrategyHTML, false},
		{"card", StrategyHTML, true}, {"card", StrategyUniversal, false},
		{"search", StrategyEmpty, true}, {"search", StrategyFreeText, false},
		{"status", StrategyEmpty, false}, {"ids", StrategyUniversal, false},
		{"nick", StrategyUniversal, false}, {"nick", StrategyEmpty, true}, {"nick_set", StrategyNull, true},
	} {
		err := fits(&ColumnRule{Kind: KindPersonal, Strategy: c.rule}, cols[c.col], false)
		if (err == nil) != c.ok {
			t.Errorf("%s %s: %v", c.col, c.rule, err)
		}
	}
	if emptyValue(cols["cc"]) != "{}" || emptyValue(cols["card"]) != "" {
		t.Error("emptyValue")
	}
}

func schemaWithLiterals() *Schema {
	s := testSchema()
	s.Tables[0].create = []string{"DROP TABLE IF EXISTS `users`",
		"CREATE TABLE `users` (`id` int, `created_at` datetime NOT NULL DEFAULT '0000-00-00 00:00:00', `email` varchar(191) COMMENT 'owner: jelena.jovanovic@firma.example')"}
	s.after = []Stmt{{SQL: "CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW `vip` AS select * from `users` where `email` = 'dragana.dragic@firma.example'", view: "vip"}}
	return s
}

func TestSchemaLiteralsNeedConfirmation(t *testing.T) {
	det, _ := pii.New(nil)
	r, _ := ParseRules([]byte(exampleRules))
	err := r.CheckSchema(schemaWithLiterals(), det)
	var ce *CheckError
	if !errors.As(err, &ce) || strings.Join(ce.Schema, "|") != "table users: email|view vip: email" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "jelena") || strings.Contains(err.Error(), "dragana") {
		t.Fatalf("a value in the error: %v", err)
	}
	// classify proposes, a person confirms.
	cls := &Classification{Schema: schemaFindings(schemaWithLiterals(), det, r)}
	raw, err := MergeRules([]byte(exampleRules), cls)
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := ParseRules(raw)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if err := proposed.CheckSchema(schemaWithLiterals(), det); !errors.As(err, &ce) || len(ce.Proposed) != 2 {
		t.Fatalf("proposals are not confirmations: %v", err)
	}
	confirmed, err := ParseRules([]byte(exampleRules + "schema:\n  table users: [email]\n  view vip:\n    kinds: [email]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := confirmed.CheckSchema(schemaWithLiterals(), det); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRules([]byte(exampleRules + "schema:\n  view vip: [name]\n")); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}

// TestSerializedAction: action: null without quotes is the action null, as
// with quotes; a missing action is the default empty-table, which leaves
// the table's rows out (an action with nothing after it is refused:
// TestRulesReject).
func TestSerializedAction(t *testing.T) {
	for in, want := range map[string]string{
		"{kind: serialized-php, action: null}":        SerializedNull,
		"{kind: serialized-php, action: ~}":           SerializedNull,
		`{kind: serialized-php, action: "null"}`:      SerializedNull,
		"{kind: serialized-php, action: empty-table}": SerializedEmptyTable,
		"{kind: serialized-php}":                      "",
	} {
		r, err := ParseRules([]byte("version: 1\ntables:\n  t:\n    columns:\n      c: " + in + "\n"))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		tr := r.Tables["t"]
		if got := tr.Columns["c"].Action; got != want {
			t.Errorf("%s: action %q, want %q", in, got, want)
		}
		if emptied := tr.emptied(); emptied != (want != SerializedNull) {
			t.Errorf("%s: emptied = %v", in, emptied)
		}
	}
}
