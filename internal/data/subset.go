package data

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sample modes of a table, the one-word forms of a SampleRule.
const (
	SampleAll  = "all"  // every row (small lookup tables)
	SampleNone = "none" // no rows
)

// Orders of the rows a sample starts from.
const (
	OrderLatest = "latest" // highest primary key first (the default)
	OrderFirst  = "first"  // lowest primary key first
)

// What happens to a nullable reference whose row is not in the sample.
const (
	NullablePull = "pull" // the referenced row is taken in (the default)
	NullableNull = "null" // the reference is set to NULL
)

// SubsetRules is the subset: section of classify.yaml: which rows data
// subset starts from, which rows it follows down, and the references the
// database does not declare. Every row the sample keeps takes in the rows its
// references point at, transitively, so the copy has no dangling reference.
type SubsetRules struct {
	// Nullable is the default for nullable references whose row is outside
	// the sample: pull (default) or null.
	Nullable string `yaml:"nullable,omitempty"`
	// Tables chooses rows per table. A table not listed has only the rows
	// other kept rows reference.
	Tables map[string]*SampleRule `yaml:"tables,omitempty"`
	// References are declared for an application without foreign keys
	// (polymorphic columns too), or change the nullable rule of a foreign key.
	References []*Reference `yaml:"references,omitempty"`
}

// SampleRule chooses the rows of one table. In YAML it is all, none, or a
// mapping. Rows, Percent and Where choose the rows the sample starts from
// (roots); Follow adds the rows that reference kept rows of other tables.
type SampleRule struct {
	Mode    string  `yaml:"-"`                 // all or none (the one-word form)
	Rows    int     `yaml:"rows,omitempty"`    // at most this many roots
	Percent float64 `yaml:"percent,omitempty"` // roots: this share of the rows, chosen by a hash of the primary key
	Where   string  `yaml:"where,omitempty"`   // roots: rows matching this SQL condition
	Order   string  `yaml:"order,omitempty"`   // latest (default) or first, by primary key
	// Follow lists parent tables: the rows of this table that reference
	// their roots or followed rows are kept too (a user's orders).
	Follow []string `yaml:"follow,omitempty"`
	// PerParent is the most followed rows per parent row (by Order); 0 is all.
	PerParent int `yaml:"per_parent,omitempty"`
}

// Reference is a reference between columns. From and To are written
// table(column, …).
type Reference struct {
	From string `yaml:"from"`
	To   string `yaml:"to,omitempty"` // omitted: From is a foreign key of the database
	// When limits a declared reference to rows whose columns have these
	// values (a polymorphic type column: commentable_type: App\Models\Post).
	When     map[string]string `yaml:"when,omitempty"`
	Nullable string            `yaml:"nullable,omitempty"` // pull or null; default subset.nullable
}

// nullableValue reads a nullable rule; a bare null in YAML is the rule null
// (decoding it into a string would give no rule at all).
func nullableValue(n yaml.Node) (string, error) {
	n = *resolve(&n)
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" && n.Value != "" {
		return NullableNull, nil
	}
	var s string
	if n.Kind == 0 {
		return "", nil
	}
	err := n.Decode(&s)
	return s, err
}

func (s *SubsetRules) UnmarshalYAML(n *yaml.Node) error {
	type plain struct {
		Nullable   yaml.Node              `yaml:"nullable,omitempty"`
		Tables     map[string]*SampleRule `yaml:"tables,omitempty"`
		References []*Reference           `yaml:"references,omitempty"`
	}
	var p plain
	if err := strictDecode(n, &p); err != nil {
		return err
	}
	nullable, err := nullableValue(p.Nullable)
	*s = SubsetRules{Nullable: nullable, Tables: p.Tables, References: p.References}
	return err
}

func (r *Reference) UnmarshalYAML(n *yaml.Node) error {
	type plain struct {
		From     string            `yaml:"from"`
		To       string            `yaml:"to,omitempty"`
		When     map[string]string `yaml:"when,omitempty"`
		Nullable yaml.Node         `yaml:"nullable,omitempty"`
	}
	var p plain
	if err := strictDecode(n, &p); err != nil {
		return err
	}
	nullable, err := nullableValue(p.Nullable)
	*r = Reference{From: p.From, To: p.To, When: p.When, Nullable: nullable}
	return err
}

func (s *SampleRule) UnmarshalYAML(n *yaml.Node) error {
	n = resolve(n)
	if n.Kind == yaml.ScalarNode {
		if n.Value != SampleAll && n.Value != SampleNone {
			return fmt.Errorf("line %d: %q is not all, none or a mapping", n.Line, n.Value)
		}
		*s = SampleRule{Mode: n.Value}
		return nil
	}
	type plain SampleRule
	var p plain
	if err := strictDecode(n, &p); err != nil {
		return err
	}
	*s = SampleRule(p)
	return nil
}

func (s *SampleRule) MarshalYAML() (any, error) {
	if s.Mode != "" {
		return s.Mode, nil
	}
	type plain SampleRule
	return (*plain)(s), nil
}

func (s *SampleRule) order() string {
	if s.Order == "" {
		return OrderLatest
	}
	return s.Order
}

// roots reports whether the rule chooses rows to start from.
func (s *SampleRule) roots() bool {
	return s.Mode == SampleAll || s.Rows > 0 || s.Percent > 0 || s.Where != ""
}

// columnRef is table(column, …) parsed.
type columnRef struct {
	table string
	cols  []string
}

func (c columnRef) String() string { return c.table + "(" + strings.Join(c.cols, ", ") + ")" }

func parseColumnRef(s string) (columnRef, error) {
	open := strings.IndexByte(s, '(')
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return columnRef{}, fmt.Errorf("%q is not table(column, …)", s)
	}
	c := columnRef{table: strings.TrimSpace(s[:open])}
	for _, col := range strings.Split(s[open+1:len(s)-1], ",") {
		if col = strings.TrimSpace(col); col == "" {
			return columnRef{}, fmt.Errorf("%q has an empty column", s)
		}
		c.cols = append(c.cols, col)
	}
	return c, nil
}

func validNullable(s string) bool { return s == "" || s == NullablePull || s == NullableNull }

// validate checks what can be checked without a schema.
func (s *SubsetRules) validate() []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, "subset: "+fmt.Sprintf(format, a...)) }
	if !validNullable(s.Nullable) {
		add("nullable must be pull or null, got %q", s.Nullable)
	}
	for _, tn := range sortedKeys(s.Tables) {
		r := s.Tables[tn]
		if r == nil {
			add("%s: empty rule (all, none or a mapping)", tn)
			continue
		}
		if r.Mode != "" {
			continue
		}
		switch {
		case r.Rows < 0 || r.PerParent < 0:
			add("%s: rows and per_parent cannot be negative", tn)
		case r.Percent < 0 || r.Percent > 100:
			add("%s: percent must be above 0 and at most 100", tn)
		case r.Order != "" && r.Order != OrderLatest && r.Order != OrderFirst:
			add("%s: order must be latest or first, got %q", tn, r.Order)
		case !r.roots() && len(r.Follow) == 0:
			add("%s: needs rows, percent, where or follow (or all, none)", tn)
		case r.PerParent > 0 && len(r.Follow) == 0:
			add("%s: per_parent needs follow", tn)
		}
	}
	for i, ref := range s.References {
		if ref == nil {
			add("references[%d]: empty", i)
			continue
		}
		from, err := parseColumnRef(ref.From)
		if err != nil {
			add("references[%d]: from: %v", i, err)
			continue
		}
		if !validNullable(ref.Nullable) {
			add("%s: nullable must be pull or null, got %q", from, ref.Nullable)
		}
		if ref.To == "" {
			if ref.When != nil {
				add("%s: when needs to (a declared reference)", from)
			}
			continue
		}
		to, err := parseColumnRef(ref.To)
		switch {
		case err != nil:
			add("%s: to: %v", from, err)
		case len(to.cols) != len(from.cols):
			add("%s: to %s has another number of columns", from, to)
		}
	}
	return errs
}

// sampleRef is one reference the closure follows: a foreign key of the
// database or a declared reference.
type sampleRef struct {
	child, parent *sampleTable
	cols          []string // in the child
	refCols       []string // in the parent
	when          []whenCond
	nullable      bool // every column of the reference is nullable
	null          bool // a parent outside the sample nulls the reference instead of being taken in
	declared      bool
	explicit      bool // the nullable rule is the reference's own, not subset.nullable

	// The closure: positions in the child's and the parent's read columns.
	at   []int
	keys *keySet // the parent's kept values of refCols

	// Following: the child follows the parent down.
	follow    bool
	perParent int
	desc      bool
	done      map[string]struct{} // parent values already followed
	pending   [][][]byte

	// The copy: positions in the copied columns of the child, nil when one is
	// generated (never written).
	copyAt []int
}

type whenCond struct {
	col   string
	value string
	at    int // in the child's read columns
	cat   int // in the child's copied columns
}

func (r *sampleRef) String() string {
	return columnRef{r.child.t.Name, r.cols}.String() + " → " + columnRef{r.parent.t.Name, r.refCols}.String()
}

// references builds the references of a schema: foreign keys, then the
// declared ones, with their nullable rules. The tables map must hold every
// table of the schema.
func (s *SubsetRules) references(schema *Schema, tables map[string]*sampleTable, e *CheckError) []*sampleRef {
	def := s.Nullable == NullableNull
	var out []*sampleRef
	byFrom := map[string]*sampleRef{}
	for _, t := range schema.Tables {
		for _, fk := range t.ForeignKeys {
			p := tables[fk.RefTable]
			if p == nil {
				continue // outside the schema that is copied
			}
			r := &sampleRef{child: tables[t.Name], parent: p, cols: fk.Columns, refCols: fk.RefColumns, null: def}
			out = append(out, r)
			byFrom[columnRef{t.Name, fk.Columns}.String()] = r
		}
	}
	for _, ref := range s.References {
		from, _ := parseColumnRef(ref.From)
		child := tables[from.table]
		if child == nil {
			e.Stale = append(e.Stale, "subset reference "+from.String())
			continue
		}
		if !hasColumns(child.t, from.cols, e, "subset reference "+from.String()) {
			continue
		}
		null := def
		if ref.Nullable != "" {
			null = ref.Nullable == NullableNull
		}
		if ref.To == "" {
			fk := byFrom[from.String()]
			if fk == nil {
				e.Invalid = append(e.Invalid, fmt.Sprintf("subset reference %s: no foreign key on these columns; declare it with to:", from))
				continue
			}
			fk.null, fk.explicit = null, ref.Nullable != ""
			continue
		}
		to, _ := parseColumnRef(ref.To)
		parent := tables[to.table]
		if parent == nil {
			e.Stale = append(e.Stale, "subset reference "+from.String()+" to "+to.String())
			continue
		}
		if !hasColumns(parent.t, to.cols, e, "subset reference to "+to.String()) {
			continue
		}
		r := &sampleRef{child: child, parent: parent, cols: from.cols, refCols: to.cols, null: null, declared: true, explicit: ref.Nullable != ""}
		ok := true
		for _, cn := range sortedKeys(ref.When) {
			if child.t.Column(cn) == nil {
				e.Stale = append(e.Stale, fmt.Sprintf("subset reference %s: when column %s", from, cn))
				ok = false
			}
			r.when = append(r.when, whenCond{col: cn, value: ref.When[cn]})
		}
		if ok {
			out = append(out, r)
		}
	}
	for _, r := range out {
		r.nullable = true
		for _, cn := range r.cols {
			if c := r.child.t.Column(cn); c == nil || !c.Nullable {
				r.nullable = false
			}
		}
		if r.null && !r.nullable {
			// subset.nullable applies to nullable references only.
			if r.explicit {
				e.Invalid = append(e.Invalid, fmt.Sprintf("subset reference %s: nullable null on a NOT NULL column", r))
			}
			r.null = false
		}
	}
	return out
}

func hasColumns(t *Table, cols []string, e *CheckError, what string) bool {
	ok := true
	for _, cn := range cols {
		if t.Column(cn) == nil {
			e.Stale = append(e.Stale, what+": column "+t.Name+"."+cn)
			ok = false
		}
	}
	return ok
}

// errNoSubset means classify.yaml has no subset: section.
var errNoSubset = errors.New("classify.yaml has no subset: section (which rows to start from)")
