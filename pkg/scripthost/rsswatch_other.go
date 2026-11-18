//go:build !linux

package scripthost

import (
	"os/exec"
	"time"
)

// probeResult mirrors the Linux one so the parent's bookkeeping compiles
// everywhere. Off Linux there is no /proc to sample: `Confine` has already
// refused the run (see confine_other.go), so this probe never carries a
// verdict of its own.
type probeResult struct {
	peak    int64
	killed  bool
	samples int
	blind   string
}

// watchRSS is a no-op off Linux: with no /proc there is nothing to sample, and
// the memory bound is already refused by Confine rather than silently weaker.
func watchRSS(_ *exec.Cmd, _ int64, _ time.Duration, _ <-chan struct{}) <-chan probeResult {
	done := make(chan probeResult, 1)
	done <- probeResult{blind: "no /proc off Linux"}
	return done
}

// markOOMVictim is a no-op off Linux: oom_score_adj is a Linux knob.
func markOOMVictim(int) error { return nil }
