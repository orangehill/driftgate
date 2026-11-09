//go:build race

package data

// raceEnabled: under the race detector the round-trip tests of the dump
// reader run fewer seeds (the fuzz step and runs without it run them all).
const raceEnabled = true
