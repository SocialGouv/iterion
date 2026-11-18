//go:build linux

package scripthost

import (
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// oomVictimScore is writable by the child's own uid and asks the kernel to
// pick THIS process first when a cgroup runs out of memory. It is a bias, not
// a bound: with it, a pod-level OOM kills the script instead of the runner that
// owns the run; without it, the kernel picks by size and may take the runner.
const oomVictimScore = 1000

// watchRSS samples the child's charged memory (see chargedBytes) and kills its
// process group when its GROWTH passes bound, because a mapping RLIMIT_DATA
// does not charge — MAP_SHARED, memfd — grows it without ever meeting the
// kernel bound that `Confine` sets. The pod's memory cgroup is where such a payload really
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
				charged, err := residentBytes(pid)
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
					r.base = charged
				}
				growth := charged - r.base
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

// residentBytes reads the child's charged memory from /proc. The parsing is
// chargedBytes, shared by every platform so a test can drive it with a
// synthetic document.
func residentBytes(pid int) (int64, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return chargedBytes(f)
}

// markOOMVictim asks the kernel to prefer this child when a cgroup is
// exhausted. Written by the PARENT, so it lands whichever interpreter the
// child runs and before any allocation it makes. The child has no descendants
// to carry it — its Landlock domain refuses exec — and a tool call is served
// by the parent's dispatcher, not inside the child.
//
// A refusal is returned, never swallowed: without the mark the kernel picks by
// size, and the pod-level OOM may take the runner instead of the script.
func markOOMVictim(pid int) error {
	return os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj",
		[]byte(strconv.Itoa(oomVictimScore)), 0o644)
}
