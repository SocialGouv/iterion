//go:build !race

package scripthost

// raceInstrumented is false: the child is an ordinary Go binary, so the tests
// exercise the production defaults as they are.
const raceInstrumented = false

// childMemoryFloor is zero without instrumentation — nothing to clear.
const childMemoryFloor int64 = 0
