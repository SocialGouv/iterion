package memlimit

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// withCgroupFiles points the derivation at fixture files for the length of
// the test.
func withCgroupFiles(t *testing.T, files ...string) {
	t.Helper()
	prev := cgroupCandidates
	cgroupCandidates = files
	t.Cleanup(func() { cgroupCandidates = prev })
}

func writeCeiling(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write ceiling fixture: %v", err)
	}
	return p
}

// TestApplyDerivesFromTheCgroupCeiling pins the derivation (#2693): the GC
// limit lands at 90% of the cgroup ceiling, and the log says where it came
// from.
func TestApplyDerivesFromTheCgroupCeiling(t *testing.T) {
	withCgroupFiles(t, writeCeiling(t, "1073741824\n")) // 1 Gi
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	var lines []string
	Apply(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	if got := debug.SetMemoryLimit(-1); got != 1073741824*90/100 {
		t.Fatalf("derived limit = %d, want %d (90%% of the 1 Gi ceiling)", got, 1073741824*90/100)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "cgroup") {
		t.Fatalf("log = %v, want one line naming the derivation", lines)
	}
}

// TestApplyOperatorsGOMEMLIMITWins pins the override rule: an explicit
// GOMEMLIMIT from the environment is never replaced by the derivation.
func TestApplyOperatorsGOMEMLIMITWins(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "256MiB")
	withCgroupFiles(t, writeCeiling(t, "1073741824\n"))
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	var lines []string
	Apply(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("the derivation replaced the operator's GOMEMLIMIT: limit = %d, was %d", got, prev)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "256MiB") {
		t.Fatalf("log = %v, want one line naming the operator's value", lines)
	}
}

// TestApplyOffDisables pins the escape hatch: ITERION_MEMLIMIT=off removes
// the derivation entirely.
func TestApplyOffDisables(t *testing.T) {
	t.Setenv(EnvDisable, "off")
	withCgroupFiles(t, writeCeiling(t, "1073741824\n"))
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	Apply(nil)
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("a disabled derivation moved the limit: %d, was %d", got, prev)
	}
}

// TestCgroupLimitReadings pins what reads as a ceiling: v2's "max", the v1
// no-limit sentinel, a parse failure and a missing file all read as NONE —
// a derivation off a nonsense number would cap the heap at it.
func TestCgroupLimitReadings(t *testing.T) {
	v1Sentinel := strconv.FormatInt(9223372036854771712, 10)
	for _, tc := range []struct {
		name    string
		content string
		want    int64
	}{
		{"v2 value", "2147483648\n", 2147483648},
		{"v2 max", "max\n", 0},
		{"v1 no-limit sentinel", v1Sentinel + "\n", 0},
		{"garbage", "a lot\n", 0},
		{"zero", "0\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCgroupFiles(t, writeCeiling(t, tc.content))
			got, _ := cgroupLimit()
			if got != tc.want {
				t.Fatalf("cgroupLimit = %d, want %d", got, tc.want)
			}
		})
	}
	// No file at all: no ceiling.
	withCgroupFiles(t, filepath.Join(t.TempDir(), "absent"))
	if got, _ := cgroupLimit(); got != 0 {
		t.Fatalf("cgroupLimit with no file = %d, want 0", got)
	}
}
