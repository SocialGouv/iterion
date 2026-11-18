package scripthost

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// probeResult is what the resident-set probe saw. It carries FACTS, so the
// verdict is a pure function of them and testable without a process.
//
// It is declared ONCE, here, for every platform. It was declared per build tag
// first, and the two copies drifted twice in one day: the Windows and darwin
// vet legs caught a field the Linux copy had and the other did not.
type probeResult struct {
	// peak is the largest GROWTH of the charged memory over the baseline the
	// probe took when the child started — never the total, which includes
	// what the child costs before its payload allocates anything.
	peak int64
	// base is the charged memory the growth is measured from.
	base int64
	// killed is true when this probe is what ended the child.
	killed bool
	// samples counts the reads that succeeded; zero means the probe was blind.
	samples int
	// blind is why the last read failed, when none ever succeeded.
	blind string
}

// chargedBytes reads, from a /proc/<pid>/status document, the memory the
// probe bounds: RssAnon + RssShmem.
//
// NOT VmRSS. VmRSS adds RssFile — the pages of the binary and of every mapped
// file, paged in as code runs and reclaimable by the kernel under pressure.
// For the iterion binary that is most of the resident set (even `cat` shows
// three quarters of its VmRSS as RssFile), it grows after any baseline as
// code is first executed, and RLIMIT_DATA never charges it: a probe reading
// VmRSS killed a legitimate run under the 64 MiB clamp floor in CI.
//
// RssShmem is the class this probe exists for — a MAP_SHARED anonymous
// mapping or a memfd is shmem-backed and charged to the pod's memory cgroup,
// but not to RLIMIT_DATA. RssAnon is the private heap, which RLIMIT_DATA
// already bounds; counting it keeps the probe's number a whole answer to
// "how much memory did this child take" rather than half of one.
//
// Both fields are required. A kernel that lacks them (they exist since 4.5;
// Landlock, which codemode requires, since 5.13) gets an error naming the
// missing field, never a silent fall back to VmRSS.
func chargedBytes(status io.Reader) (int64, error) {
	var anon, shmem int64
	var haveAnon, haveShmem bool
	sc := bufio.NewScanner(status)
	for sc.Scan() {
		line := sc.Text()
		var dst *int64
		switch {
		case strings.HasPrefix(line, "RssAnon:"):
			dst, haveAnon = &anon, true
		case strings.HasPrefix(line, "RssShmem:"):
			dst, haveShmem = &shmem, true
		default:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("scripthost: status line has no value: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("scripthost: status value %q: %w", fields[1], err)
		}
		*dst = kb * 1024
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	switch {
	case !haveAnon:
		return 0, fmt.Errorf("scripthost: the status document carries no RssAnon line")
	case !haveShmem:
		return 0, fmt.Errorf("scripthost: the status document carries no RssShmem line")
	}
	return anon + shmem, nil
}
