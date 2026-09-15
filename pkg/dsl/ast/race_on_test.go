//go:build race

package ast_test

// raceEnabled is true under the race detector, where a timing witness
// measures the detector, not the code.
const raceEnabled = true
