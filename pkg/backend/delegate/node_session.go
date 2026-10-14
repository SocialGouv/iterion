package delegate

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// What a delegated agent starts ends with its delegation
// (docs/adr/127-an-agent-nodes-processes-end-with-the-node.md): the spawn
// exports the delegation's node session variable, every descendant inherits
// it, and the end of the delegation stops the session — the processes that
// carry it and their descendants. In a sandbox the token is the spawn's mark
// (wrapSandboxDelegateArgv exports it, killSandboxDelegate ends it); elsewhere
// it is drawn here and swept from this process's /proc.

// sweepOwner is the delegation a sweep ends, and where the sweep says what it
// did: the run log, and the delegation's background-work hook — what a node
// left running is background work lost with its end, a delegate_background
// event like the work a claude_code session loses.
type sweepOwner struct {
	logger  *iterlog.Logger
	who     string
	backend string
	hook    func(BackgroundWork)
}

func ownerOf(logger *iterlog.Logger, task Task, backend string) sweepOwner {
	return sweepOwner{
		logger:  logger,
		who:     fmt.Sprintf("%s#%d/%s", task.NodeID, task.Iteration, backend),
		backend: backend,
		hook:    task.Hooks.OnBackgroundWork,
	}
}

// The reasons a sweep's background-work event gives: what it stopped, and
// what it could not do when it stopped nothing.
const (
	nodeSessionEndReason    = "the node ended, and the processes it left running were stopped with it"
	nodeSessionUnsureReason = "the node ended, and what it left could not all be stopped or looked at"
)

// withNodeSessionEnv returns a copy of env carrying token's variable: the map
// a spawn builder receives may be shared with the next spawn.
func withNodeSessionEnv(env map[string]string, token string) map[string]string {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out[proc.NodeSessionVar(token)] = "1"
	return out
}

// NodeSessionSince is when a session begins, for the spawns that do not keep
// a spawnEnd: zero — every unreadable process of this uid is then reported,
// more than the session's, never less — when this platform cannot tell.
func NodeSessionSince() proc.BootTicks {
	since, _ := proc.BootTicksNow()
	return since
}

var unsupportedSweepOnce sync.Once

// SweepLocalNodeSession stops the session token of task's delegation to
// backend in this process's PID namespace — the host's, or the run
// container's for an agent loop that runs there — says what it did, and
// returns the labels of what it stopped. since is when the session began.
func SweepLocalNodeSession(logger *iterlog.Logger, task Task, backend, token string, since proc.BootTicks) []string {
	r, err := proc.SweepNodeSession(token, since)
	if errors.Is(err, proc.ErrNodeSessionSweepUnsupported) {
		// A property of the platform, not of the node: said once.
		unsupportedSweepOnce.Do(func() {
			logger.Warn("node session: %v — what an agent node starts outlives it on this host (docs/adr/127-an-agent-nodes-processes-end-with-the-node.md)", err)
		})
		err = nil
	} else if err != nil {
		err = fmt.Errorf("the processes this node left could not all be found: %w", err)
	}
	return reportNodeSessionSweep(ownerOf(logger, task, backend), r, err)
}

// reportNodeSessionSweep says what a sweep did — what it stopped, what it
// could not stop or could not look at, and sweepErr when it could not look at
// all or did not finish — in the run log and as a background work event, and
// returns the labels a delegation records with the background work it lost.
// Every sweep reports through here.
func reportNodeSessionSweep(o sweepOwner, r proc.SweepReport, sweepErr error) []string {
	labels := r.Labels()
	problems := errors.Join(r.Problems(), sweepErr)
	if len(labels) > 0 {
		o.logger.Warn("[%s] node session: the node left %d process(es) running; stopped at its end: %s",
			o.who, len(labels), strings.Join(labels, "; "))
	}
	if problems != nil {
		o.logger.Warn("[%s] node session: %v", o.who, problems)
	}
	// The event is the node's: what it left and the sweep stopped, what still
	// runs after KILL, and a sweep that could not look or did not finish. A
	// process whose environment no one can read may be anyone's of this uid
	// started meanwhile — the operator's, another node's — and stays in the
	// run log.
	eventProblems := sweepErr
	if len(r.Survivors) > 0 {
		eventProblems = errors.Join(proc.SweepReport{Survivors: r.Survivors}.Problems(), sweepErr)
	}
	if o.hook != nil && (len(labels) > 0 || eventProblems != nil) {
		reason := nodeSessionEndReason
		switch {
		case eventProblems != nil && len(labels) == 0:
			reason = nodeSessionUnsureReason + ": " + eventProblems.Error()
		case eventProblems != nil:
			reason += "; " + eventProblems.Error()
		}
		o.hook(BackgroundWork{Backend: o.backend, Phase: BackgroundAbandoned, Running: len(labels), Tasks: labels, Reason: reason})
	}
	return labels
}

// spawnEnd is the end of one delegated spawn. A delegation runs it where the
// result is assembled, and again on every exit path as a safety net: it ends
// the spawn the first time it runs after the CLI was spawned, and returns nil
// otherwise — a node refused before its CLI ever started has nothing to end
// and spends no exec on it.
type spawnEnd struct {
	end     func() []string
	since   atomic.Uint64
	spawned atomic.Bool
	once    sync.Once
}

// started records that the spawn happened and, the first time, when; the
// command builder calls it on every attempt. The time is stored before the
// flag, so whoever sees the flag sees the time.
func (s *spawnEnd) started() {
	s.since.CompareAndSwap(0, uint64(NodeSessionSince()))
	s.spawned.Store(true)
}

// sinceStart is when the first attempt was spawned.
func (s *spawnEnd) sinceStart() proc.BootTicks { return proc.BootTicks(s.since.Load()) }

// run ends the spawn and returns the labels of what it stopped.
func (s *spawnEnd) run() []string {
	if !s.spawned.Load() {
		return nil
	}
	var out []string
	s.once.Do(func() { out = s.end() })
	return out
}

// SandboxNodeSession prepares a sandboxed spawn of task's delegation to
// backend to carry its node session, for a backend that builds its own exec:
// wrap turns the spawn's argv into the pidfile-writing wrapper that exports
// the session, and end ends the spawn — the recorded process killed, the
// session swept — and returns the labels of what it stopped.
func SandboxNodeSession(run sandbox.Run, task Task, backend string, logger *iterlog.Logger) (wrap func([]string) []string, end func() []string) {
	mark := sandboxDelegateMark(task)
	return func(argv []string) []string { return wrapSandboxDelegateArgv(mark, argv) },
		killSandboxDelegateReporting(run, mark, ownerOf(logger, task, backend))
}
