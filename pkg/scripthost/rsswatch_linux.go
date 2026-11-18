//go:build linux

package scripthost

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// oomVictimScore is writable by the child's own uid and asks the kernel to
// pick THIS process first when a cgroup runs out of memory. It is a bias, not
// a bound: with it, a pod-level OOM kills the script instead of the runner that
// owns the run; without it, the kernel picks by size and may take the runner.
const oomVictimScore = 1000

// probeResult is what the resident-set probe saw. It carries FACTS, so the
// verdict below is a pure function of them and testable without a process.
type probeResult struct {
	// peak is the largest GROWTH of the resident set over the baseline the
	// probe took when the child started — not the total. The total includes
	// the Go runtime, the binary's resident code and the interpreter, which at
	// a 64 MiB bound is most of the budget by itself: measured in CI, a
	// legitimate run under the clamp floor was killed at 68665344 bytes
	// against a 67108864-byte bound, having grown by far less than that.
	peak    int64
	base    int64  // the resident set the growth is measured from
	killed  bool   // this probe is what ended the child
	samples int    // how many reads succeeded; zero means the probe was blind
	blind   string // why the last read failed, when none ever succeeded
}

// watchRSS samples the child's resident set and kills its process group when it
// passes bound, because a mapping RLIMIT_DATA does not charge — MAP_SHARED,
// memfd — grows the resident set without ever meeting the kernel bound that
// `Confine` sets. The pod's memory cgroup is where such a payload really
// stops, and a cgroup kill picks its victim by size: the probe makes the child
// the victim deliberately rather than leaving it to the OOM killer's choice.
//
// The returned channel yields exactly once: when the probe kills, or when stop
// closes (the child ended on its own). bound <= 0 means "nobody chose", and
// then nothing is sampled.
func watchRSS(cmd *exec.Cmd, bound int64, interval time.Duration, stop <-chan struct{}) <-chan probeResult {
	done := make(chan probeResult, 1)
	if cmd == nil || cmd.Process == nil || bound <= 0 {
		done <- probeResult{}
		return done
	}
	go func() {
		var r probeResult
		r.base = -1
		pid := cmd.Process.Pid
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				done <- r
				return
			case <-tick.C:
				rss, err := residentBytes(pid)
				if err != nil {
					// The child is gone, or /proc is not mounted: either way
					// there is nothing to compare. Kept for the case where
					// NOTHING was ever read — a probe that never sampled must
					// not read as a probe that found nothing.
					r.blind = err.Error()
					continue
				}
				r.samples++
				r.blind = ""
				// The FIRST reading is the baseline: what the child costs
				// before its payload allocates anything. Growth from there is
				// the quantity RLIMIT_DATA would have bounded. A payload that
				// allocated everything before this sample would set its own
				// baseline — 20 ms is early enough for a child that must read
				// its request and start an interpreter, and the kernel's own
				// RLIMIT still bounds the heap meanwhile.
				if r.base < 0 {
					r.base = rss
				}
				growth := rss - r.base
				if growth > r.peak {
					r.peak = growth
				}
				if growth > bound {
					if _, kerr := proc.KillProcessGroup(cmd); kerr != nil {
						r.blind = "kill: " + kerr.Error()
						done <- r
						return
					}
					r.killed = true
					done <- r
					return
				}
			}
		}
	}()
	return done
}

// residentBytes reads VmRSS out of /proc/<pid>/status. It is the page-cache-
// free resident size: everything the process actually holds in RAM, including
// shared and file-backed mappings that RLIMIT_DATA never sees.
func residentBytes(pid int) (int64, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("scripthost: VmRSS line has no value: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("scripthost: VmRSS value %q: %w", fields[1], err)
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("scripthost: /proc/%d/status carries no VmRSS line", pid)
}

// markOOMVictim asks the kernel to prefer this child when a cgroup is
// exhausted. Written by the PARENT, so it lands whichever interpreter the
// child runs and before any allocation it makes; descendants inherit it, which
// is what covers a native payload a tool call spawns.
//
// A refusal is returned, never swallowed: without the mark the kernel picks by
// size, and the pod-level OOM may take the runner instead of the script.
func markOOMVictim(pid int) error {
	return os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj",
		[]byte(strconv.Itoa(oomVictimScore)), 0o644)
}
