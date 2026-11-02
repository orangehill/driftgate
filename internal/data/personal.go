package data

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/pii"
)

// PersonalValue is one original personal value of the source, for the leak
// check of data verify: none of them may appear anywhere in the output.
type PersonalValue struct {
	Table, Column string
	// Kind is the strategy of a whole value (universal, email, token,
	// password, null, empty), or for a part of free text, HTML, JSON or
	// serialized PHP the detector's kind (email, phone, jmbg, card, iban) or
	// the strategy of its JSON path.
	Kind  string
	Value string
	// Category is the category of the column (CatText, CatNumeric…); a
	// number inside a JSON value is CatNumeric.
	Category string

	ahead any // what eachPersonal's ahead made of it
}

// PersonalValues streams the original personal values of the source to fn,
// table by table and row by row in the same order as Anonymize: whole values
// of personal columns, the values under personal JSON paths, and what the
// detector finds in free text, HTML, JSON and serialized PHP. Rows of
// emptied and skipped tables never reach the output and are not read.
//
// The values are originals: fn should hash them right away (Replacer.Digest)
// and never write them anywhere. Short values (an initial, a two-digit
// number) may appear in any output by chance; the caller decides which are
// long enough to count.
func PersonalValues(ctx context.Context, src Database, rules *Rules, det *pii.Detector, fn func(PersonalValue) error) error {
	dl, err := dialectFor(src.Driver)
	if err != nil {
		return err
	}
	if det == nil {
		if det, err = pii.New(nil); err != nil {
			return err
		}
	}
	_, err = inSnapshot(ctx, src, rules, func(c *sql.Conn, schema *Schema) error {
		return eachPersonal(ctx, c, dl, det, schema, rules, nil, fn)
	})
	return err
}

// inSnapshot reads the schema of a source in a read-only snapshot, checks
// the rules against it and runs fn in the same snapshot.
func inSnapshot(ctx context.Context, src Database, rules *Rules, fn func(c *sql.Conn, schema *Schema) error) (*Schema, error) {
	dl, err := dialectFor(src.Driver)
	if err != nil {
		return nil, err
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
	if err := rules.Check(schema, true); err != nil {
		return nil, err
	}
	return schema, fn(c, schema)
}

// eachPersonal streams the original personal values of every table that
// is not emptied (see PersonalValues). ahead, when not nil, runs on the
// workers that find the values, before fn gets each; its result is in the
// value (PersonalValue.ahead).
func eachPersonal(ctx context.Context, c *sql.Conn, dl dialect, det *pii.Detector, s *Schema, rules *Rules, ahead func(PersonalValue) any, fn func(PersonalValue) error) error {
	for _, t := range s.Tables {
		tr := rules.Tables[t.Name]
		if tr == nil || tr.emptied() {
			continue
		}
		if err := personalRows(ctx, c, dl, det, t, tr, ahead, fn); err != nil {
			return err
		}
	}
	return nil
}

func personalRows(ctx context.Context, c *sql.Conn, dl dialect, det *pii.Detector, t *Table, tr *TableRule, ahead func(PersonalValue) any, fn func(PersonalValue) error) error {
	var cols []*Column
	var rules []*ColumnRule
	for _, col := range t.Columns {
		cr := tr.Columns[col.Name]
		if col.Generated || cr.Kind == KindKeep || cr.Kind == KindFile {
			continue
		}
		cols = append(cols, col)
		rules = append(rules, cr)
	}
	if len(cols) == 0 {
		return nil
	}
	// The detector runs ahead on a worker per CPU; fn gets the values in
	// row order, as from one reader.
	err := readAhead(ctx, c, dl.selectRows(t, cols, ""), len(cols), func(b *rowBatch) error {
		vs, err := personalIn(det, t, cols, rules, b.rows)
		if ahead != nil {
			for i := range vs {
				vs[i].ahead = ahead(vs[i])
			}
		}
		b.work = vs
		return err
	}, func(b *rowBatch) error {
		for _, v := range b.work.([]PersonalValue) {
			if err := fn(v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("source %s: %w", t.Name, err)
	}
	return nil
}

// personalIn finds the original personal values of rows.
func personalIn(det *pii.Detector, t *Table, cols []*Column, rules []*ColumnRule, rows [][][]byte) ([]PersonalValue, error) {
	var out []PersonalValue
	for _, row := range rows {
		for i, raw := range row {
			if raw == nil {
				continue
			}
			if err := personalOf(det, t, cols[i], rules[i], string(raw), &out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// personalOf appends the original personal values of one value.
func personalOf(det *pii.Detector, t *Table, col *Column, cr *ColumnRule, s string, out *[]PersonalValue) error {
	emit := func(kind, v string, number bool) error {
		if v == "" {
			return nil
		}
		cat := col.Category
		if number {
			cat = CatNumeric
		}
		*out = append(*out, PersonalValue{Table: t.Name, Column: col.Name, Kind: kind, Value: v, Category: cat})
		return nil
	}
	switch {
	case cr.Kind == KindSerializedPHP, cr.Strategy == StrategyFreeText, cr.Strategy == StrategyHTML:
		return emitSpans(det, s, emit)
	case cr.Strategy == StrategyJSON:
		return emitJSON(det, s, newJSONRule(cr), emit)
	case isArray(col):
		for _, e := range arrayElements(s) {
			emit(cr.Strategy, e, false)
		}
		return nil
	}
	return emit(cr.Strategy, s, false)
}

func emitSpans(det *pii.Detector, s string, emit func(kind, v string, number bool) error) error {
	for _, sp := range det.Spans(s) {
		if err := emit(sp.Kind, s[sp.Start:sp.End], false); err != nil {
			return err
		}
	}
	return nil
}

func emitJSON(det *pii.Detector, raw string, rule *jsonRule, emit func(kind, v string, number bool) error) error {
	v, err := jsonx.Parse([]byte(raw))
	if err != nil {
		if rule.other == KindKeep {
			return nil
		}
		return emitSpans(det, raw, emit)
	}
	var walk func(path string, v *jsonx.Value, strat string) error
	walk = func(path string, v *jsonx.Value, strat string) error {
		if s, ok := rule.match(path); ok {
			strat = s
		}
		switch v.Kind {
		case jsonx.Object:
			for _, m := range v.Members {
				var err error
				switch strat {
				case "":
					if rule.other == StrategyFreeText {
						err = emitSpans(det, m.Key, emit)
					}
				case KindKeep:
				case StrategyFreeText, StrategyHTML:
					err = emitSpans(det, m.Key, emit)
				default:
					err = emit(strat, m.Key, false)
				}
				if err != nil {
					return err
				}
				if err := walk(jsonx.KeyPath(path, m.Key), m.Value, strat); err != nil {
					return err
				}
			}
		case jsonx.Array:
			for _, item := range v.Items {
				if err := walk(path+"[*]", item, strat); err != nil {
					return err
				}
			}
		case jsonx.String:
			switch strat {
			case KindKeep:
			case "":
				if rule.other == StrategyFreeText {
					return emitSpans(det, v.Text, emit)
				}
			case StrategyFreeText, StrategyHTML:
				return emitSpans(det, v.Text, emit)
			default:
				return emit(strat, v.Text, false)
			}
		case jsonx.Int, jsonx.Float:
			// As anonymize replaces them: under a path that is not keep, and
			// under no path when the key names a personal identifier.
			switch strat {
			case KindKeep:
			case "":
				if rule.other == StrategyFreeText && personalNumber(path, v.Text) {
					return emit(StrategyUniversal, v.Text, true)
				}
			case StrategyFreeText, StrategyHTML, StrategyEmail, StrategyPassword:
				return emit(StrategyUniversal, v.Text, true)
			default:
				return emit(strat, v.Text, true)
			}
		}
		return nil
	}
	return walk("$", v, "")
}
