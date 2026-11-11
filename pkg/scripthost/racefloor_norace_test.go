//go:build !race

package scripthost

// childMemoryFloor is zero without instrumentation — nothing to clear.
const childMemoryFloor int64 = 0
