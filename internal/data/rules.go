package data

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/pii"
)

// RulesVersion is the version of classify.yaml this build reads.
const RulesVersion = 1

// Column kinds.
const (
	KindKeep          = "keep"           // copied as it is
	KindPersonal      = "personal"       // replaced by a strategy
	KindSerializedPHP = "serialized-php" // PHP serialize() output: the table is emptied or the column nulled
	KindFile          = "file"           // a path to a file; the file gets a placeholder (FilePlaceholder)
)

// Strategies of personal columns.
const (
	StrategyUniversal = "universal"
	StrategyEmail     = "email"
	StrategyPassword  = "password"
	StrategyToken     = "token"
	StrategyFreeText  = "free-text"
	StrategyHTML      = "html"
	StrategyJSON      = "json"
	StrategyNull      = "null"
	StrategyEmpty     = "empty"
)

var strategies = []string{StrategyUniversal, StrategyEmail, StrategyPassword, StrategyToken, StrategyFreeText, StrategyHTML, StrategyJSON, StrategyNull, StrategyEmpty}

// Table actions.
const (
	ActionCopy  = "copy"  // rows are copied, every column by its rule (the default)
	ActionEmpty = "empty" // the structure is copied without rows
	ActionSkip  = "skip"  // the table is left out entirely
)

// Serialized PHP actions.
const (
	SerializedEmptyTable = "empty-table"
	SerializedNull       = "null"
)

// Rules is classify.yaml: a rule for every column of every table, written
// by data classify as a proposal and confirmed by a person.
type Rules struct {
	Version int                   `yaml:"version"`
	Tables  map[string]*TableRule `yaml:"tables"`
	// Subset chooses the rows of data subset; data anonymize copies every row.
	Subset *SubsetRules `yaml:"subset,omitempty"`
	// Schema confirms what the detector finds in schema objects (comments,
	// defaults, views, triggers, functions, checks), which are copied as
	// they are: by object, the kinds a person checked and found not personal
	// (a shared mailbox in a default). Anything else found stops a run.
	Schema map[string]*SchemaRule `yaml:"schema,omitempty"`
}

// SchemaRule confirms the detector's findings in one schema object. In YAML
// it is a list of kinds, or a mapping with kinds, reason and proposed.
type SchemaRule struct {
	Kinds    []string `yaml:"kinds"`
	Reason   string   `yaml:"reason,omitempty"`
	Proposed bool     `yaml:"proposed,omitempty"`
}

func (c *SchemaRule) UnmarshalYAML(n *yaml.Node) error {
	n = resolve(n)
	if n.Kind == yaml.SequenceNode {
		*c = SchemaRule{}
		return n.Decode(&c.Kinds)
	}
	type plain SchemaRule
	var p plain
	if err := strictDecode(n, &p); err != nil {
		return err
	}
	*c = SchemaRule(p)
	return nil
}

func (c *SchemaRule) MarshalYAML() (any, error) {
	if c.Reason == "" && !c.Proposed {
		return c.Kinds, nil
	}
	type plain SchemaRule
	return (*plain)(c), nil
}

// confirmed reports whether a person confirmed a kind in an object.
func (r *Rules) confirmed(object, kind string) bool {
	sr := r.Schema[object]
	if sr == nil || sr.Proposed {
		return false
	}
	for _, k := range sr.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// TableRule is the rule of one table and its columns.
type TableRule struct {
	Action   string                 `yaml:"action,omitempty"`
	Reason   string                 `yaml:"reason,omitempty"`
	Proposed bool                   `yaml:"proposed,omitempty"`
	Columns  map[string]*ColumnRule `yaml:"columns,omitempty"`
}

// ColumnRule is the rule of one column. In YAML it is either a mapping or,
// for the plain cases, one word: keep, file, serialized-php or the name of a
// strategy (which implies kind personal).
type ColumnRule struct {
	Kind     string            `yaml:"kind"`
	Strategy string            `yaml:"strategy,omitempty"`
	Paths    map[string]string `yaml:"paths,omitempty"`   // json: JSON path glob → strategy (or keep)
	Other    string            `yaml:"other,omitempty"`   // json: strings under no path; default free-text
	Action   string            `yaml:"action,omitempty"`  // serialized-php: empty-table (default) or null
	Options  map[string]string `yaml:"options,omitempty"` // file: passed to the FilePlaceholder
	Reason   string            `yaml:"reason,omitempty"`
	Proposed bool              `yaml:"proposed,omitempty"` // written by data classify; a person removes it
}

func (c *ColumnRule) UnmarshalYAML(n *yaml.Node) error {
	n = resolve(n)
	if n.Kind == yaml.ScalarNode {
		switch n.Value {
		case "":
			return fmt.Errorf("line %d: empty rule", n.Line)
		case KindKeep, KindFile, KindSerializedPHP:
			*c = ColumnRule{Kind: n.Value}
		default:
			*c = ColumnRule{Kind: KindPersonal, Strategy: n.Value}
		}
		return nil
	}
	type plain ColumnRule
	var p plain
	if err := strictDecode(n, &p); err != nil {
		return err
	}
	*c = ColumnRule(p)
	// action: null written without quotes is the action null, as nullable
	// reads it (decoding it into a string would give "", the default
	// empty-table, and empty the table instead). action: with nothing after
	// it, or an empty string, names no action and is refused: read as the
	// default, it would drop every row. Only a missing action is the default.
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k, v := n.Content[i], resolve(n.Content[i+1]); k.Value == "action" && v.Kind == yaml.ScalarNode {
				switch {
				case v.Tag == "!!null" && v.Value != "":
					c.Action = SerializedNull
				case v.Value == "":
					return fmt.Errorf("line %d: an empty action names none (%s or %s)", v.Line, SerializedEmptyTable, SerializedNull)
				}
			}
		}
	}
	if c.Kind == "" && c.Strategy != "" {
		c.Kind = KindPersonal
	}
	return nil
}

func (t *TableRule) UnmarshalYAML(n *yaml.Node) error {
	// The columns are decoded one by one: a bare null (the strategy null
	// written without quotes) would otherwise be no rule at all.
	type plain struct {
		Action   string               `yaml:"action,omitempty"`
		Reason   string               `yaml:"reason,omitempty"`
		Proposed bool                 `yaml:"proposed,omitempty"`
		Columns  map[string]yaml.Node `yaml:"columns,omitempty"`
	}
	var p plain
	if err := strictDecode(n, &p); err != nil {
		return err
	}
	*t = TableRule{Action: p.Action, Reason: p.Reason, Proposed: p.Proposed}
	if p.Columns != nil {
		t.Columns = map[string]*ColumnRule{}
	}
	for name, v := range p.Columns {
		v := *resolve(&v)
		if v.Kind == yaml.ScalarNode && v.Tag == "!!null" && v.Value != "" {
			t.Columns[name] = &ColumnRule{Kind: KindPersonal, Strategy: StrategyNull}
			continue
		}
		var c ColumnRule
		if err := c.UnmarshalYAML(&v); err != nil {
			return fmt.Errorf("column %s: %w", name, err)
		}
		t.Columns[name] = &c
	}
	return nil
}

// strictDecode decodes a mapping and rejects keys the struct does not have
// (Node.Decode alone ignores them).
func strictDecode(n *yaml.Node, v any) error {
	n = resolve(n)
	if n.Kind == yaml.MappingNode {
		known := map[string]bool{}
		rt := reflect.TypeOf(v).Elem()
		for i := 0; i < rt.NumField(); i++ {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("yaml"), ",")
			known[name] = true
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i].Value; !known[k] {
				return fmt.Errorf("line %d: unknown key %q", n.Content[i].Line, k)
			}
		}
	}
	return n.Decode(v)
}

// short is the one-word form of a rule, or "" when it needs a mapping.
func (c *ColumnRule) short() string {
	if c.Paths != nil || c.Other != "" || c.Action != "" || c.Options != nil || c.Reason != "" || c.Proposed {
		return ""
	}
	if c.Kind == KindPersonal {
		return c.Strategy
	}
	return c.Kind
}

func (c *ColumnRule) MarshalYAML() (any, error) {
	if s := c.short(); s != "" {
		return s, nil
	}
	type plain ColumnRule
	return (*plain)(c), nil
}

// LoadRules reads classify.yaml; a file that does not exist gives empty
// rules (every column then counts as unclassified).
func LoadRules(path string) (*Rules, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Rules{Version: RulesVersion, Tables: map[string]*TableRule{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseRules(raw)
}

// ParseRules reads the rules from YAML; an unknown key is an error.
func ParseRules(raw []byte) (*Rules, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	r := &Rules{}
	if err := dec.Decode(r); err != nil {
		return nil, fmt.Errorf("classify.yaml: %w", err)
	}
	if r.Version != RulesVersion {
		return nil, fmt.Errorf("classify.yaml: unsupported version %d (this build reads version %d)", r.Version, RulesVersion)
	}
	if r.Tables == nil {
		r.Tables = map[string]*TableRule{}
	}
	for name, t := range r.Tables {
		if t == nil {
			r.Tables[name] = &TableRule{}
		}
	}
	return r, r.validate()
}

func (r *Rules) validate() error {
	var errs []string
	for _, tn := range sortedKeys(r.Tables) {
		t := r.Tables[tn]
		switch t.Action {
		case "", ActionCopy, ActionEmpty, ActionSkip:
		default:
			errs = append(errs, fmt.Sprintf("%s: unknown action %q (copy, empty, skip)", tn, t.Action))
		}
		for _, cn := range sortedKeys(t.Columns) {
			if t.Columns[cn] == nil {
				errs = append(errs, fmt.Sprintf("%s.%s: empty rule", tn, cn))
				continue
			}
			if err := t.Columns[cn].validate(); err != nil {
				errs = append(errs, fmt.Sprintf("%s.%s: %v", tn, cn, err))
			}
		}
	}
	if r.Subset != nil {
		errs = append(errs, r.Subset.validate()...)
	}
	for _, obj := range sortedKeys(r.Schema) {
		sr := r.Schema[obj]
		if sr == nil || len(sr.Kinds) == 0 {
			errs = append(errs, fmt.Sprintf("schema: %s: no kinds", obj))
			continue
		}
		for _, k := range sr.Kinds {
			if !pii.Kind(k) {
				errs = append(errs, fmt.Sprintf("schema: %s: unknown kind %q (%s)", obj, k, strings.Join(pii.Kinds, ", ")))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("classify.yaml:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func knownStrategy(s string) bool {
	for _, k := range strategies {
		if k == s {
			return true
		}
	}
	return false
}

func (c *ColumnRule) validate() error {
	switch c.Kind {
	case KindKeep, KindFile:
		if c.Strategy != "" || c.Paths != nil || c.Other != "" || c.Action != "" {
			return fmt.Errorf("kind %s takes no strategy, paths, other or action", c.Kind)
		}
		if c.Options != nil {
			return fmt.Errorf("kind %s takes no options", c.Kind)
		}
	case KindSerializedPHP:
		switch c.Action {
		case "", SerializedEmptyTable, SerializedNull:
		default:
			return fmt.Errorf("serialized-php action must be %s or %s", SerializedEmptyTable, SerializedNull)
		}
		if c.Strategy != "" || c.Paths != nil || c.Other != "" || c.Options != nil {
			return errors.New("kind serialized-php takes only action")
		}
	case KindPersonal:
		if !knownStrategy(c.Strategy) {
			return fmt.Errorf("unknown strategy %q (%s)", c.Strategy, strings.Join(strategies, ", "))
		}
		if c.Action != "" || c.Options != nil {
			return errors.New("a personal column takes no action or options")
		}
		if c.Strategy != StrategyJSON && (c.Paths != nil || c.Other != "") {
			return errors.New("paths and other are for strategy json")
		}
		for p, s := range c.Paths {
			if !strings.HasPrefix(p, "$") {
				return fmt.Errorf("path %q must start with $", p)
			}
			if !jsonLeafStrategy(s) {
				return fmt.Errorf("path %s: strategy %q cannot be used inside JSON (keep, universal, email, token, password, free-text, html, null, empty)", p, s)
			}
		}
		if c.Other != "" && c.Other != KindKeep && c.Other != StrategyFreeText {
			return errors.New("other must be keep or free-text")
		}
	case "":
		return errors.New("needs kind (keep, personal, serialized-php, file) or a strategy")
	default:
		return fmt.Errorf("unknown kind %q (keep, personal, serialized-php, file)", c.Kind)
	}
	return nil
}

func jsonLeafStrategy(s string) bool {
	return s == KindKeep || (knownStrategy(s) && s != StrategyJSON)
}

// tableAction is the action of a table with its default.
func (t *TableRule) action() string {
	if t == nil || t.Action == "" {
		return ActionCopy
	}
	return t.Action
}

// emptied reports whether the rows of a table are left out: by its action
// or by a serialized PHP column whose rule empties the table.
func (t *TableRule) emptied() bool {
	if t.action() != ActionCopy {
		return true
	}
	for _, c := range t.Columns {
		if c.Kind == KindSerializedPHP && (c.Action == "" || c.Action == SerializedEmptyTable) {
			return true
		}
	}
	return false
}

// CheckError lists everything that stops a run before it writes anything.
type CheckError struct {
	Unclassified []string // table.column without a rule (e.g. new after a migration)
	Stale        []string // rules for tables or columns that no longer exist
	Proposed     []string // rules data classify proposed and nobody confirmed yet
	Invalid      []string // rules that do not fit the column (type, NOT NULL, references)
	Schema       []string // schema objects with personal-looking literals nobody confirmed
}

func (e *CheckError) Error() string {
	var b strings.Builder
	b.WriteString("classify.yaml does not fit the database:")
	part := func(title string, list []string) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n  %s (%d):", title, len(list))
		for _, s := range list {
			b.WriteString("\n    " + s)
		}
	}
	part("unclassified columns, run 'driftgate data classify'", e.Unclassified)
	part("proposed rules not confirmed yet (review, then remove proposed: true)", e.Proposed)
	part("rules for tables or columns that do not exist", e.Stale)
	part("rules that do not fit", e.Invalid)
	part("schema objects with what looks like personal data, copied as they are: change the schema, or confirm the kinds under schema: (data classify proposes them)", e.Schema)
	return b.String()
}

// Check matches the rules against a schema: every column of every copied
// table must have a rule that fits it, and no rule may name a table or column
// the schema does not have. files says whether file rules can be served.
func (r *Rules) Check(s *Schema, files bool) error {
	e := &CheckError{}
	for _, t := range s.Tables {
		tr := r.Tables[t.Name]
		if tr == nil {
			for _, c := range t.Columns {
				e.Unclassified = append(e.Unclassified, t.Name+"."+c.Name)
			}
			continue
		}
		if tr.Proposed {
			e.Proposed = append(e.Proposed, t.Name)
		}
		for _, cn := range sortedKeys(tr.Columns) {
			if t.Column(cn) == nil {
				e.Stale = append(e.Stale, t.Name+"."+cn)
			}
		}
		if tr.action() != ActionCopy {
			continue
		}
		for _, c := range t.Columns {
			cr := tr.Columns[c.Name]
			if cr == nil {
				e.Unclassified = append(e.Unclassified, t.Name+"."+c.Name)
				continue
			}
			if cr.Proposed {
				e.Proposed = append(e.Proposed, t.Name+"."+c.Name)
			}
			if err := fits(cr, c, files); err != nil {
				e.Invalid = append(e.Invalid, fmt.Sprintf("%s.%s: %v", t.Name, c.Name, err))
			}
		}
	}
	for _, tn := range sortedKeys(r.Tables) {
		if s.Table(tn) == nil {
			e.Stale = append(e.Stale, tn)
		}
	}
	// A table without rows (or without the table itself) cannot be referenced
	// by rows that are copied.
	for _, t := range s.Tables {
		tr := r.Tables[t.Name]
		if tr == nil || tr.emptied() {
			continue
		}
		for _, fk := range t.ForeignKeys {
			parent := r.Tables[fk.RefTable]
			if parent == nil || !parent.emptied() {
				continue
			}
			nullable := true
			for _, cn := range fk.Columns {
				if c := t.Column(cn); c == nil || !c.Nullable {
					nullable = false
				}
			}
			nulled := nullable
			for _, cn := range fk.Columns {
				if cr := tr.Columns[cn]; cr == nil || cr.Kind != KindPersonal || cr.Strategy != StrategyNull {
					nulled = false
				}
			}
			if !nulled {
				e.Invalid = append(e.Invalid, fmt.Sprintf("%s(%s) references %s, which is emptied or skipped: empty %s as well or null the column", t.Name, strings.Join(fk.Columns, ", "), fk.RefTable, t.Name))
			}
		}
	}
	e.Invalid = append(e.Invalid, r.referenceRules(s)...)
	if len(e.Unclassified)+len(e.Stale)+len(e.Proposed)+len(e.Invalid) > 0 {
		return e
	}
	return nil
}

// referenceRules checks that both ends of every reference (a foreign key,
// or one declared under subset.references) are written alike: the same value
// must get the same stand-in in both tables, or the rows stop joining
// without an error anywhere. A referencing column may be nulled instead.
func (r *Rules) referenceRules(s *Schema) []string {
	var out []string
	check := func(what string, from, to columnRef) {
		ft, tt := r.Tables[from.table], r.Tables[to.table]
		if ft == nil || tt == nil || ft.emptied() || tt.emptied() || len(from.cols) != len(to.cols) {
			return // unclassified, or emptied (checked above)
		}
		for i, cn := range from.cols {
			child, parent := ft.Columns[cn], tt.Columns[to.cols[i]]
			if child == nil || parent == nil || sameRule(child, parent) {
				continue
			}
			out = append(out, fmt.Sprintf("%s.%s (%s) references %s.%s (%s) by %s: give both ends the same rule, or null %s.%s",
				from.table, cn, child.describe(), to.table, to.cols[i], parent.describe(), what, from.table, cn))
		}
	}
	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			check("a foreign key", columnRef{t.Name, fk.Columns}, columnRef{fk.RefTable, fk.RefColumns})
		}
	}
	if r.Subset != nil {
		for _, ref := range r.Subset.References {
			if ref.To == "" {
				continue // a foreign key of the database, checked above
			}
			from, err1 := parseColumnRef(ref.From)
			to, err2 := parseColumnRef(ref.To)
			if err1 == nil && err2 == nil {
				check("a reference declared in subset.references", from, to)
			}
		}
	}
	return out
}

// sameRule reports whether a referencing column (child) gets the same
// stand-ins as the column it references (parent).
func sameRule(child, parent *ColumnRule) bool {
	if child.Kind == KindPersonal && child.Strategy == StrategyNull {
		return true
	}
	return child.Kind == parent.Kind && child.Strategy == parent.Strategy && child.Action == parent.Action &&
		reflect.DeepEqual(child.Paths, parent.Paths) && child.Other == parent.Other && reflect.DeepEqual(child.Options, parent.Options)
}

func (c *ColumnRule) describe() string {
	if c.Kind == KindPersonal {
		return c.Strategy
	}
	return c.Kind
}

// CheckSchema runs the detector over the schema objects that are copied as
// they are (table definitions with their comments and defaults, views,
// triggers, functions, constraints): what it finds must be confirmed under
// schema: in classify.yaml, or the run stops before it writes anything.
func (r *Rules) CheckSchema(s *Schema, det *pii.Detector) error {
	e := &CheckError{}
	for _, f := range schemaFindings(s, det, r) {
		if sr := r.Schema[f.Object]; sr != nil && sr.Proposed {
			e.Proposed = append(e.Proposed, "schema: "+f.Object)
			continue
		}
		var missing []string
		for _, k := range f.Kinds {
			if !r.confirmed(f.Object, k) {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			e.Schema = append(e.Schema, f.Object+": "+strings.Join(missing, ", "))
		}
	}
	if len(e.Schema)+len(e.Proposed) > 0 {
		return e
	}
	return nil
}

// fits reports whether a rule can be applied to a column of its type.
func fits(cr *ColumnRule, c *Column, files bool) error {
	switch cr.Kind {
	case KindKeep:
		return nil
	case KindFile:
		if !files {
			return errors.New("a file column needs file placeholders: set data.files.source and data.files.target in driftgate.yaml")
		}
		if c.Category != CatText {
			return fmt.Errorf("a file column must hold text (it is %s)", c.Type)
		}
		return nil
	case KindSerializedPHP:
		if cr.Action == SerializedNull && !c.Nullable {
			return errors.New("action null on a NOT NULL column")
		}
		if c.Category != CatText && c.Category != CatBinary {
			return fmt.Errorf("serialized PHP must be text or binary (it is %s)", c.Type)
		}
		return nil
	}
	ok := map[string][]string{
		StrategyUniversal: {CatText, CatNumeric},
		StrategyToken:     {CatText, CatNumeric},
		StrategyEmail:     {CatText},
		StrategyPassword:  {CatText},
		StrategyFreeText:  {CatText},
		StrategyHTML:      {CatText},
		StrategyJSON:      {CatText, CatJSON},
		StrategyEmpty:     {CatText, CatBinary},
	}
	switch {
	case cr.Strategy == StrategyNull:
		if !c.Nullable {
			return errors.New("strategy null on a NOT NULL column")
		}
		return nil
	case c.nondeterministic && cr.Strategy != StrategyEmpty:
		return fmt.Errorf("collation %s is nondeterministic: values it counts as equal would get stand-ins it counts as different (use keep, null or empty)", c.Collation)
	}
	for _, cat := range ok[cr.Strategy] {
		if c.Category == cat {
			return nil
		}
	}
	for _, st := range otherStrategies(c) {
		if st == cr.Strategy {
			return nil
		}
	}
	if s := otherStrategies(c); len(s) > 0 {
		return fmt.Errorf("strategy %s does not fit type %s (use keep, null, %s)", cr.Strategy, c.Type, strings.Join(s, ", "))
	}
	return fmt.Errorf("strategy %s does not fit type %s (use keep or null)", cr.Strategy, c.Type)
}

// otherStrategies are the strategies besides keep and null for a column of
// another type that holds text (textual): an array of text is replaced
// element by element, XML as HTML (markup kept), and the others can only
// be emptied.
func otherStrategies(c *Column) []string {
	switch t := strings.ToLower(c.Type); {
	case !textual(c):
		return nil
	case isArray(c):
		return []string{StrategyUniversal, StrategyEmail, StrategyToken, StrategyFreeText, StrategyEmpty}
	case t == "xml":
		return []string{StrategyHTML, StrategyFreeText, StrategyEmpty}
	case t == "tsvector", t == "hstore":
		return []string{StrategyEmpty}
	}
	return nil // MySQL enum and set: values of the schema itself
}

// emptyValue is what strategy empty writes: an empty text, or an empty
// array.
func emptyValue(c *Column) string {
	if isArray(c) {
		return "{}"
	}
	return ""
}

// Marshal writes the rules as YAML, tables and columns sorted.
func (r *Rules) Marshal() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolve follows an alias (*name) to the node it names, so that a check of
// a node's kind or tag sees the value written at the anchor: an alias of a
// bare null is that null, not a node of another kind.
func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}
