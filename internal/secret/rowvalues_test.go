package secret

import (
	"bytes"
	"testing"
)

// TestRowValues: a row value is named by an HMAC under the project key:
// the same value of one column gets the same name, another column,
// table or key another one, and the name is not a plain hash of the value.
func TestRowValues(t *testing.T) {
	k1, k2 := Key(bytes.Repeat([]byte{1}, KeySize)), Key(bytes.Repeat([]byte{2}, KeySize))
	r1, r2 := k1.RowValues(), k2.RowValues()
	a := r1.Name("db/table:follows", "target", "v")
	if a != k1.RowValues().Name("db/table:follows", "target", "v") || len(a) != 32 {
		t.Fatalf("not stable: %s", a)
	}
	for _, other := range []string{
		r1.Name("db/table:follows", "other", "v"),
		r1.Name("db/table:users", "target", "v"),
		r1.Name("db/table:follows", "target", "w"),
		r2.Name("db/table:follows", "target", "v"),
		r1.Name("db/table:follows", "targe", "tv"), // fields are delimited
	} {
		if other == a {
			t.Errorf("equal names for different inputs: %s", a)
		}
	}
	if r1.ID() == r2.ID() || r1.ID() != k1.RowValues().ID() {
		t.Error("the key IDs do not tell the keys apart")
	}
}
