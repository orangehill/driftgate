package lazy

import "testing"

func TestRegexpCompilesOnce(t *testing.T) {
	re := Regexp(`^a+$`)
	first := re()
	if !first.MatchString("aaa") || first.MatchString("b") {
		t.Fatal("wrong pattern")
	}
	if re() != first {
		t.Fatal("compiled again")
	}
}

func TestRegexpPanicsOnUse(t *testing.T) {
	re := Regexp(`(`) // must not panic here
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for a bad pattern")
		}
	}()
	re()
}
