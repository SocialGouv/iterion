// Package memlimit derives the Go GC's memory limit from the container's
// cgroup ceiling at startup (#2693), so the collector works harder as the
// pod nears its limit instead of meeting the OOM-killer first — the
// complement of the DSL-read admission bound, which keeps concurrent
// readings from reaching it.
//
// An explicit GOMEMLIMIT from the operator always wins (the runtime applies
// it before this package runs, and nothing here overrides it);
// ITERION_MEMLIMIT=off removes the derivation — the greppable escape hatch
// every load-bearing bound keeps. A host with no cgroup ceiling (a dev
// machine, an unlimited container) is left exactly as it was.
package memlimit

import (
	"os"
	"strconv"
	"strings"

	"runtime/debug"
)

// EnvDisable disables the cgroup derivation when set to "off" (any case,
// whitespace tolerated).
const EnvDisable = "ITERION_MEMLIMIT"

// headroom is the share of the cgroup ceiling left to non-heap memory
// (stacks, the runtime, C-go, page cache pressure the kernel attributes):
// the GC is asked to hold the heap under 90% of the pod's limit.
const headroomDivisor100 = 90

// cgroupCandidates are the files the ceiling is read from: cgroup v2, then
// v1. A value of "max" (v2), 0, or a number past the sanity floor — the
// kernel's no-limit sentinel is near int64's top — reads as no ceiling.
var cgroupCandidates = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

// Apply derives and installs the GC limit. logf receives one line naming
// the outcome — the derivation, the operator's win, or the opt-out — so the
// pod's boot log says which GOMEMLIMIT it runs under. Nil silences it.
func Apply(logf func(format string, args ...any)) {
	if v := strings.TrimSpace(os.Getenv("GOMEMLIMIT")); v != "" {
		if logf != nil {
			logf("memlimit: GOMEMLIMIT=%s from the environment is in force — no cgroup derivation", v)
		}
		return
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvDisable)), "off") {
		if logf != nil {
			logf("memlimit: %s=off — no GOMEMLIMIT derived from the cgroup ceiling", EnvDisable)
		}
		return
	}
	limit, source := cgroupLimit()
	if limit <= 0 {
		if logf != nil {
			logf("memlimit: no cgroup memory ceiling found — GOMEMLIMIT left unset")
		}
		return
	}
	derived := limit * headroomDivisor100 / 100
	debug.SetMemoryLimit(derived)
	if logf != nil {
		logf("memlimit: GOMEMLIMIT derived from %s: %d bytes (90%% of the %d-byte cgroup ceiling; GOMEMLIMIT overrides, %s=off disables)", source, derived, limit, EnvDisable)
	}
}

// cgroupLimit reads the container's memory ceiling, trying cgroup v2 then
// v1. (0, "") when there is none.
func cgroupLimit() (int64, string) {
	for _, path := range cgroupCandidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(data))
		if v == "" || v == "max" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		// The kernel's "no limit" sits at the top of int64 (v1 writes
		// 9223372036854771712); treat anything past 1 PiB as unbounded
		// rather than deriving a limit on a number that means none.
		if err != nil || n <= 0 || n > 1<<50 {
			continue
		}
		return n, path
	}
	return 0, ""
}
