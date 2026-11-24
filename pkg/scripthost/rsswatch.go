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
	f, err := statusBytes(status, "RssAnon", "RssShmem")
	if err != nil {
		return 0, err
	}
	return f["RssAnon"] + f["RssShmem"], nil
}

// statusBytes reads the named kB fields of a /proc/<pid>/status document, in
// bytes. Every name is required: a missing one is an error that names it,
// never a zero that would read as "nothing held". It is the one reader of that
// document — the probe's charge and the child's own data segment both go
// through it, so the two cannot drift into parsing it differently.
func statusBytes(status io.Reader, names ...string) (map[string]int64, error) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	got := make(map[string]int64, len(names))
	sc := bufio.NewScanner(status)
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok || !want[name] {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			return nil, fmt.Errorf("scripthost: status line has no value: %q", line)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("scripthost: status value %q: %w", fields[0], err)
		}
		got[name] = kb * 1024
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, n := range names {
		if _, ok := got[n]; !ok {
			return nil, fmt.Errorf("scripthost: the status document carries no %s line", n)
		}
	}
	return got, nil
}
