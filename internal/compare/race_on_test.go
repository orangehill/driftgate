//go:build race

package compare

// raceEnabled: the race detector allocates on its own, so allocation
// budgets are not checked under it (CI checks them in a run without it).
const raceEnabled = true
