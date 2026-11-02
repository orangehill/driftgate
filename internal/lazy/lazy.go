// Package lazy defers package-level setup to first use, so a command pays
// only for the patterns and tables of the code it runs (`driftgate version`
// compiles no regular expression).
package lazy

import (
	"regexp"
	"sync"
)

// Regexp returns a function that compiles expr on its first call and returns
// the same *regexp.Regexp on every call. Like regexp.MustCompile, it panics
// if expr does not compile.
func Regexp(expr string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(expr) })
}
