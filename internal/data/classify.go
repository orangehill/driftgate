package data

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/pii"
)

// Classification is what data classify found: rules to propose for tables
// and columns that have none, rules for what no longer exists, and proposals
// nobody confirmed yet. It never holds a sampled value.
type Classification struct {
	Tables      []TableProposal
	Stale       []Ref
	Unconfirmed []Ref
	// Schema proposes confirming what the detector finds in schema objects;
	// StaleSchema are confirmations for objects where it finds nothing now.
	Schema      []SchemaFinding
	StaleSchema []string
}

// SchemaFinding is what the detector finds in one schema object, by kind,
// with a reason in counts.
type SchemaFinding struct {
	Object string
	Kinds  []string
	Reason string
}

// Ref names a table, or a column of it.
type Ref struct{ Table, Column string }

func (r Ref) String() string {
	if r.Column == "" {
		return r.Table
	}
	return r.Table + "." + r.Column
}

// TableProposal holds the proposals for one table.
type TableProposal struct {
	Table   string
	New     bool   // the table has no rule at all
	Action  string // proposed table action; "" copies
	Reason  string
	Columns []ColumnProposal // in the order of the table
}

// ColumnProposal is a proposed rule; it carries proposed: true and a reason
// with counts only.
type ColumnProposal struct {
	Column string
	Rule   *ColumnRule
}

// Changed reports whether the rules need a change.
func (c *Classification) Changed() bool {
	return len(c.Tables)+len(c.Stale)+len(c.Schema)+len(c.StaleSchema) > 0
}

// Classify proposes rules for every table and column of the source that
// classify.yaml does not cover, from the names and types of the columns and
// from up to sample values of each (in primary-key order, so the proposal
// is the same every time). Existing rules are never changed.
func Classify(ctx context.Context, src Database, rules *Rules, det *pii.Detector, sample int) (*Classification, error) {
	dl, err := dialectFor(src.Driver)
	if err != nil {
		return nil, err
	}
	if det == nil {
		if det, err = pii.New(nil); err != nil {
			return nil, err
		}
	}
	db, err := open(src, src.Name)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := dl.session(ctx, c); err != nil {
		return nil, err
	}
	if err := dl.snapshot(ctx, c); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	schema, err := dl.schema(ctx, c, src.Name)
	if err != nil {
		return nil, err
	}
	out := &Classification{}
	for _, tn := range sortedKeys(rules.Tables) {
		tr := rules.Tables[tn]
		t := schema.Table(tn)
		if t == nil {
			out.Stale = append(out.Stale, Ref{Table: tn})
			continue
		}
		if tr.Proposed {
			out.Unconfirmed = append(out.Unconfirmed, Ref{Table: tn})
		}
		for _, cn := range sortedKeys(tr.Columns) {
			switch {
			case t.Column(cn) == nil:
				out.Stale = append(out.Stale, Ref{tn, cn})
			case tr.Columns[cn].Proposed:
				out.Unconfirmed = append(out.Unconfirmed, Ref{tn, cn})
			}
		}
	}
	for _, t := range schema.Tables {
		tr := rules.Tables[t.Name]
		if tr != nil && tr.action() != ActionCopy {
			continue
		}
		var missing []*Column
		for _, col := range t.Columns {
			if tr == nil || tr.Columns[col.Name] == nil {
				missing = append(missing, col)
			}
		}
		if len(missing) == 0 {
			continue
		}
		tp := TableProposal{Table: t.Name, New: tr == nil}
		if tp.New {
			if reason := transientTable(t.Name); reason != "" && len(schema.References(t.Name)) == 0 {
				tp.Action, tp.Reason = ActionEmpty, reason
				out.Tables = append(out.Tables, tp)
				continue
			}
		}
		samples, err := sampleRows(ctx, c, dl, t, missing, sample)
		if err != nil {
			return nil, err
		}
		for i, col := range missing {
			r := propose(t, col, samples[i], det)
			r.Proposed = true
			tp.Columns = append(tp.Columns, ColumnProposal{Column: col.Name, Rule: r})
		}
		out.Tables = append(out.Tables, tp)
	}
	found := map[string]bool{}
	for _, f := range schemaFindings(schema, det, rules) {
		found[f.Object] = true
		if sr := rules.Schema[f.Object]; sr != nil && sr.Proposed {
			out.Unconfirmed = append(out.Unconfirmed, Ref{Table: "schema: " + f.Object})
			continue
		}
		for _, k := range f.Kinds {
			if !rules.confirmed(f.Object, k) {
				out.Schema = append(out.Schema, f)
				break
			}
		}
	}
	for _, obj := range sortedKeys(rules.Schema) {
		if !found[obj] {
			out.StaleSchema = append(out.StaleSchema, obj)
		}
	}
	return out, nil
}

// schemaText is a statement that recreates part of the schema, copied as it
// is, and the object it makes ("table users", "view vip", "constraint
// users.users_phone_check").
type schemaText struct{ object, sql string }

func (s *Schema) texts() []schemaText {
	var out []schemaText
	add := func(q string) { out = append(out, schemaText{objectName(q), q}) }
	for _, st := range s.before {
		add(st.SQL)
	}
	for _, t := range s.Tables {
		for _, q := range t.create {
			add(q)
		}
	}
	for _, st := range s.after {
		add(st.SQL)
	}
	return out
}

const sqlName = "(?:\"[^\"]*\"|`[^`]*`|[\\w$]+)(?:\\.(?:\"[^\"]*\"|`[^`]*`|[\\w$]+))*"

var (
	createObject = lazy.Regexp(`(?is)^CREATE\s+(?:OR\s+REPLACE\s+)?(?:ALGORITHM\s*=\s*\w+\s+)?(?:SQL\s+SECURITY\s+\w+\s+)?(?:UNIQUE\s+)?` +
		`(TABLE|VIEW|TRIGGER|FUNCTION|PROCEDURE|EVENT|SEQUENCE|INDEX|TYPE|SCHEMA|EXTENSION)\s+(?:IF\s+NOT\s+EXISTS\s+)?(` + sqlName + `)`)
	addConstraint = lazy.Regexp(`(?is)^ALTER\s+TABLE\s+(?:ONLY\s+)?(` + sqlName + `)\s+ADD\s+CONSTRAINT\s+(` + sqlName + `)`)
	alterSequence = lazy.Regexp(`(?is)^ALTER\s+SEQUENCE\s+(` + sqlName + `)`)
)

// objectName names the object a schema statement makes, the same way for
// the source, an output database and a dump.
func objectName(q string) string {
	unquote := func(n string) string {
		n = strings.NewReplacer(`"`, "", "`", "").Replace(n)
		return strings.TrimPrefix(n, "public.")
	}
	if m := createObject().FindStringSubmatch(q); m != nil {
		return strings.ToLower(m[1]) + " " + unquote(m[2])
	}
	if m := addConstraint().FindStringSubmatch(q); m != nil {
		return "constraint " + unquote(m[1]) + "." + unquote(m[2])
	}
	if m := alterSequence().FindStringSubmatch(q); m != nil {
		return "sequence " + unquote(m[1])
	}
	return "statement"
}

// schemaFindings runs the detector over the schema statements, by object
// in name order.
func schemaFindings(s *Schema, det *pii.Detector, r *Rules) []SchemaFinding {
	counts := map[string]map[string]int{}
	for _, st := range s.texts() {
		for _, sp := range det.Spans(st.sql) {
			if counts[st.object] == nil {
				counts[st.object] = map[string]int{}
			}
			counts[st.object][sp.Kind]++
		}
	}
	var out []SchemaFinding
	for _, obj := range sortedKeys(counts) {
		f := SchemaFinding{Object: obj, Kinds: sortedKeys(counts[obj])}
		var parts []string
		for _, k := range f.Kinds {
			parts = append(parts, fmt.Sprintf("%d %s", counts[obj][k], k))
		}
		f.Reason = "proposed: the detector finds " + strings.Join(parts, ", ") + " in its definition, which is copied as it is; " +
			"remove proposed: true only if none of it is personal, otherwise change the schema"
		out = append(out, f)
	}
	return out
}

// transientTable recognises tables of sessions, caches and queues, which
// applications rebuild themselves.
func transientTable(name string) string {
	switch name {
	case "sessions", "cache", "cache_locks", "jobs", "job_batches", "failed_jobs", "password_resets", "password_reset_tokens":
		return "proposed: sessions, cache, queue or reset tokens; the application rebuilds them"
	}
	if strings.HasPrefix(name, "telescope_") {
		return "proposed: debugging records (Laravel Telescope)"
	}
	return ""
}

// textual reports a column of another type whose values are text people
// write, read as text: PostgreSQL arrays of text, xml, tsvector, hstore;
// MySQL enum and set.
func textual(c *Column) bool {
	if c.Category != CatOther {
		return false
	}
	t := strings.ToLower(c.Type)
	if isArray(c) {
		t = strings.TrimSuffix(t, "[]")
		for _, p := range []string{"text", "character", "char", "varchar", "citext", "name", "json", "jsonb", "xml"} {
			if t == p || strings.HasPrefix(t, p+"(") || strings.HasPrefix(t, p+" ") {
				return true
			}
		}
		return false
	}
	return t == "xml" || t == "tsvector" || t == "hstore" || strings.HasPrefix(t, "enum(") || strings.HasPrefix(t, "set(")
}

// isArray reports a PostgreSQL array column.
func isArray(c *Column) bool { return c.Category == CatOther && strings.HasSuffix(c.Type, "[]") }

// sampleRows reads up to n non-NULL values of each column, as text.
func sampleRows(ctx context.Context, c *sql.Conn, dl dialect, t *Table, cols []*Column, n int) ([][]string, error) {
	out := make([][]string, len(cols))
	var read []*Column
	var at []int
	for i, col := range cols {
		if col.Category == CatText || col.Category == CatJSON || col.Category == CatBinary || textual(col) {
			read = append(read, col)
			at = append(at, i)
		}
	}
	if len(read) == 0 || n <= 0 {
		return out, nil
	}
	rows, err := c.QueryContext(ctx, dl.selectRows(t, read, "")+fmt.Sprintf(" LIMIT %d", n))
	if err != nil {
		return nil, fmt.Errorf("sample %s: %w", t.Name, err)
	}
	defer rows.Close()
	raw := make([]sql.RawBytes, len(read))
	ptrs := make([]any, len(read))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, r := range raw {
			if len(r) > 0 {
				out[at[i]] = append(out[at[i]], string(r))
			}
		}
	}
	return out, rows.Err()
}

var (
	serializedPHP = lazy.Regexp(`^(a:\d+:\{|O:\d+:"|C:\d+:"|s:\d+:"|i:-?\d+;|b:[01];|d:[^;]+;|N;)`)
	htmlTag       = lazy.Regexp(`<(p|br|div|span|a|b|i|strong|em|ul|ol|li|h[1-6]|table|tr|td|img)\b[^>]*>`)
	wholeEmail    = lazy.Regexp(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}$`)
	passwordHash  = lazy.Regexp(`^(\$2[aby]\$\d\d\$|\$argon2i?d?\$|pbkdf2_sha256\$)`)
)

// Name hints: the column name split into words.
var (
	emailWords    = []string{"email", "e_mail", "mail"}
	passwordWords = []string{"password", "passwd", "pwd", "lozinka"}
	tokenWords    = []string{"token", "secret", "api_key", "apikey", "otp"}
	nameWords     = []string{"first_name", "last_name", "firstname", "lastname", "surname", "full_name", "fullname", "username", "user_name",
		"nickname", "display_name", "middle_name", "maiden_name", "ime", "prezime", "ime_prezime", "contact_person", "kontakt_osoba"}
	phoneWords   = []string{"phone", "mobile", "tel", "telephone", "telefon", "fax", "mob", "mobilni"}
	addressWords = []string{"address", "street", "adresa", "ulica", "house_number", "address_line"}
	idWords      = []string{"jmbg", "pib", "oib", "ssn", "maticni_broj", "passport", "iban", "account_number", "bank_account", "racun",
		"card_number", "tax_id", "vat_number", "lk", "licna_karta", "birth_date", "date_of_birth", "dob", "datum_rodjenja"}
	ipWords     = []string{"ip", "ip_address", "ipaddress", "last_ip", "remote_addr"}
	peopleWords = []string{"user", "customer", "member", "client", "person", "people", "contact", "employee", "author", "korisnik",
		"klijent", "kupac", "clan", "zaposleni", "subscriber", "applicant", "patient", "student", "guest", "owner"}
)

// hint reports whether the name is one of the words, or ends with _word.
func hint(name string, list []string) bool {
	n := strings.ToLower(name)
	for _, w := range list {
		if n == w || strings.HasSuffix(n, "_"+w) || strings.HasPrefix(n, w+"_") {
			return true
		}
	}
	return false
}

func peopleTable(table string) bool {
	t := strings.ToLower(table)
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	for _, w := range peopleWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

func share(n, of int) bool { return of > 0 && n*5 >= of*4 } // at least 80 %

// propose picks a rule for one column. The reason names the evidence in
// counts, never a value.
func propose(t *Table, c *Column, samples []string, det *pii.Detector) *ColumnRule {
	personal := func(strategy, why string) *ColumnRule {
		return &ColumnRule{Kind: KindPersonal, Strategy: strategy, Reason: "proposed: " + why}
	}
	keep := func(why string) *ColumnRule { return &ColumnRule{Kind: KindKeep, Reason: "proposed: " + why} }
	n := len(samples)
	count := func(f func(string) bool) int {
		k := 0
		for _, s := range samples {
			if f(s) {
				k++
			}
		}
		return k
	}
	if k := count(func(s string) bool { return serializedPHP().MatchString(s) }); n > 0 && share(k, n) && c.Category != CatJSON {
		return &ColumnRule{Kind: KindSerializedPHP, Reason: fmt.Sprintf("proposed: %d of %d sampled values are serialized PHP; the table is emptied (or set action: null)", k, n)}
	}
	switch c.Category {
	case CatOther, CatBinary:
		return proposeOther(c, samples, det, personal, keep)
	case CatNumeric:
		if hint(c.Name, phoneWords) || hint(c.Name, idWords) {
			return personal(StrategyUniversal, "a number that is personal by its name")
		}
		return keep("a number")
	case CatJSON:
		return &ColumnRule{Kind: KindPersonal, Strategy: StrategyJSON, Reason: "proposed: JSON; strings are treated as free text, add paths for personal fields"}
	}
	switch {
	case hint(c.Name, ipWords):
		if c.Nullable {
			return personal(StrategyNull, "an IP address by its name")
		}
		return personal(StrategyUniversal, "an IP address by its name (digits replaced; not a valid address afterwards)")
	case hint(c.Name, emailWords):
		return personal(StrategyEmail, "an e-mail address by its name")
	case hint(c.Name, passwordWords):
		return personal(StrategyPassword, "a password hash by its name")
	case hint(c.Name, tokenWords) || strings.HasSuffix(strings.ToLower(c.Name), "_token"):
		return personal(StrategyToken, "a token or secret by its name")
	case hint(c.Name, nameWords), strings.EqualFold(c.Name, "name") && peopleTable(t.Name):
		return personal(StrategyUniversal, "a person's name by its name")
	case hint(c.Name, phoneWords), hint(c.Name, addressWords), hint(c.Name, idWords):
		return personal(StrategyUniversal, "personal by its name")
	}
	if n == 0 {
		if strings.EqualFold(c.Name, "name") {
			return keep("a name, but the table does not look like people, and no values to sample: check")
		}
		return keep("no values to sample: check")
	}
	if k := count(func(s string) bool { return wholeEmail().MatchString(s) }); share(k, n) {
		return personal(StrategyEmail, fmt.Sprintf("%d of %d sampled values are e-mail addresses", k, n))
	}
	if k := count(func(s string) bool { return passwordHash().MatchString(s) }); share(k, n) {
		return personal(StrategyPassword, fmt.Sprintf("%d of %d sampled values are password hashes", k, n))
	}
	if k := count(func(s string) bool {
		v, err := jsonx.Parse([]byte(s))
		return err == nil && (v.Kind == jsonx.Object || v.Kind == jsonx.Array)
	}); share(k, n) {
		return personal(StrategyJSON, fmt.Sprintf("%d of %d sampled values are JSON; strings are treated as free text, add paths for personal fields", k, n))
	}
	if k := count(htmlTag().MatchString); k > 0 {
		return personal(StrategyHTML, fmt.Sprintf("%d of %d sampled values hold HTML", k, n))
	}
	found, whole, total, longest := 0, 0, 0, 0
	for _, s := range samples {
		sp := det.Spans(s)
		if len(sp) > 0 {
			found++
			if len(sp) == 1 && sp[0].Start == 0 && sp[0].End == len(s) {
				whole++
			}
		}
		total += len(s)
		longest = max(longest, len(s))
	}
	switch {
	case found > 0 && share(whole, found):
		return personal(StrategyUniversal, fmt.Sprintf("the detector finds personal data in %d of %d sampled values, each the whole value", found, n))
	case found > 0:
		return personal(StrategyFreeText, fmt.Sprintf("the detector finds personal data in %d of %d sampled values", found, n))
	case strings.EqualFold(c.Name, "name"):
		return keep("a name, but the table does not look like people: check")
	case total/n > 80 || longest > 255:
		return personal(StrategyFreeText, "free text (long values); only detected personal data is replaced")
	}
	return keep(fmt.Sprintf("nothing personal in %d sampled values", n))
}

// proposeOther picks a rule for a column of another type or of bytes. Text
// in it (an array of text, XML, tsvector, bytes that are UTF-8 text) is
// sampled like text: with personal data found it gets a strategy the type
// can take (otherStrategies), else null, else empty; without, keep, which a
// person confirms like every proposal.
func proposeOther(c *Column, samples []string, det *pii.Detector, personal func(string, string) *ColumnRule, keep func(string) *ColumnRule) *ColumnRule {
	found, read, emails := 0, 0, 0
	for _, s := range samples {
		if c.Category == CatBinary {
			if s = binaryText(s); !printable([]byte(s)) {
				continue
			}
		}
		read++
		parts := []string{s}
		if isArray(c) {
			parts = arrayElements(s)
		}
		hit, email := false, len(parts) > 0
		for _, p := range parts {
			sp := det.Spans(p)
			hit = hit || len(sp) > 0
			email = email && len(sp) == 1 && sp[0].Kind == "email" && sp[0].Start == 0 && sp[0].End == len(p)
		}
		if hit {
			found++
		}
		if email {
			emails++
		}
	}
	byName := hint(c.Name, idWords) || hint(c.Name, ipWords)
	if found == 0 && !byName {
		if read > 0 {
			return keep(fmt.Sprintf("nothing personal in %d sampled values of type %s", read, c.Type))
		}
		return keep("type " + c.Type)
	}
	why := "personal by its name"
	if found > 0 {
		why = fmt.Sprintf("the detector finds personal data in %d of %d sampled values", found, read)
	}
	can := otherStrategies(c)
	has := func(st string) bool {
		for _, x := range can {
			if x == st {
				return true
			}
		}
		return false
	}
	switch {
	case found > 0 && isArray(c) && share(emails, found):
		return personal(StrategyEmail, why+", each element an e-mail address; replaced element by element")
	case found > 0 && has(StrategyFreeText) && !has(StrategyHTML):
		return personal(StrategyFreeText, why+"; only detected personal data is replaced")
	case found > 0 && has(StrategyHTML):
		return personal(StrategyHTML, why+"; XML: text and attribute values replaced, markup kept")
	case c.Nullable:
		return personal(StrategyNull, why+", of a type that cannot be replaced ("+c.Type+")")
	case has(StrategyEmpty) || c.Category == CatBinary:
		return personal(StrategyEmpty, why+", of a type that cannot be replaced ("+c.Type+") and NOT NULL")
	}
	return keep(why + ", but of a type that cannot be replaced (" + c.Type + ") and NOT NULL: decide")
}

const rulesHeader = `Rules of driftgate data: every column of every table. data classify proposes
rules (proposed: true); a person checks each, removes proposed: true and commits.
A column without a rule stops data anonymize.`

// MergeRules adds the proposals to classify.yaml and removes the rules for
// what no longer exists; every other rule, and its comments, stays as it is.
func MergeRules(raw []byte, c *Classification) ([]byte, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("classify.yaml: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, HeadComment: rulesHeader}}}
		root := doc.Content[0]
		root.Content = append(root.Content, scalar("version"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"})
	}
	root := doc.Content[0]
	tables := child(root, "tables", true)
	tables.Style = 0
	for _, s := range c.Stale {
		if s.Column == "" {
			remove(tables, s.Table)
			continue
		}
		if t := child(tables, s.Table, false); t != nil {
			if cols := child(t, "columns", false); cols != nil {
				remove(cols, s.Column)
			}
		}
	}
	for _, tp := range c.Tables {
		t := child(tables, tp.Table, false)
		if t == nil {
			t = &yaml.Node{Kind: yaml.MappingNode}
			tables.Content = append(tables.Content, scalar(tp.Table), t)
			if tp.Action != "" {
				t.Content = append(t.Content, scalar("action"), scalar(tp.Action), scalar("reason"), scalar(tp.Reason),
					scalar("proposed"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
			}
		}
		if len(tp.Columns) == 0 {
			continue
		}
		cols := child(t, "columns", true)
		for _, cp := range tp.Columns {
			var n yaml.Node
			if err := n.Encode(cp.Rule); err != nil {
				return nil, err
			}
			n.Style = yaml.FlowStyle
			cols.Content = append(cols.Content, scalar(cp.Column), &n)
		}
	}
	sortPairs(tables)
	if len(c.Schema)+len(c.StaleSchema) > 0 {
		sch := child(root, "schema", true)
		for _, obj := range c.StaleSchema {
			remove(sch, obj)
		}
		for _, f := range c.Schema {
			remove(sch, f.Object)
			var n yaml.Node
			if err := n.Encode(&SchemaRule{Kinds: f.Kinds, Reason: f.Reason, Proposed: true}); err != nil {
				return nil, err
			}
			sch.Content = append(sch.Content, scalar(f.Object), &n)
		}
		sortPairs(sch)
		if len(sch.Content) == 0 {
			remove(root, "schema")
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if _, err := ParseRules(b.Bytes()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

// child finds the value of a key in a mapping, or adds an empty mapping.
func child(m *yaml.Node, key string, create bool) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind != yaml.MappingNode && create {
				*v = yaml.Node{Kind: yaml.MappingNode}
			}
			return v
		}
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, scalar(key), v)
	return v
}

func remove(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// sortPairs sorts a mapping by key, keeping each value (and its comments)
// with its key.
func sortPairs(m *yaml.Node) {
	type pair struct{ k, v *yaml.Node }
	var ps []pair
	for i := 0; i+1 < len(m.Content); i += 2 {
		ps = append(ps, pair{m.Content[i], m.Content[i+1]})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].k.Value < ps[j].k.Value })
	m.Content = m.Content[:0]
	for _, p := range ps {
		m.Content = append(m.Content, p.k, p.v)
	}
}
