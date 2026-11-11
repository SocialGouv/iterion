//go:build race

package scripthost

import (
	"os"
	"strconv"
)

// childMemoryFloor is what a race-instrumented child needs just to start.
// ThreadSanitizer's allocations count against RLIMIT_DATA, so a test's memory
// bound has to clear them before the script runs at all.
//
// MEASURED on an amd64 Linux host, not guessed: 288, 320 and 384 MiB all die
// at startup ("failed to allocate TracePart"), 448 MiB starts. 768 MiB is that
// floor with room, and the override exists so the next person who sees this
// fail can re-measure in one command instead of editing a constant.
var childMemoryFloor = func() int64 {
	if raw := os.Getenv("ITERION_SCRIPTHOST_RACE_FLOOR_MIB"); raw != "" {
		if mib, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return mib << 20
		}
	}
	return 768 << 20
}()
