// Package chaos names the points where the crash tests (tools/e2e) stop a
// driftgate process: each is a moment at which a kill must leave the store,
// the baselines and the databases in a state the next command recovers
// from. Outside those tests nothing is called there.
package chaos

// At, when set, is called at every point with its name. Only the crash
// tests set it, in the process they then kill; the program never does.
var At func(point string)

// Point marks a point.
func Point(name string) {
	if At != nil {
		At(name)
	}
}
