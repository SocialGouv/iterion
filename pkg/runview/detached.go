package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// envDetached opts the editor server into spawning each run as a
// detached `iterion run --background` subprocess instead of running
// the engine in-process. Surviving the editor's own restarts (e.g.
// watchexec rebuilds during dev) is the entire goal; the flag exists
// so the path can soak behind an operator-controlled toggle before
// becoming the default.
const envDetached = "ITERION_RUNS_DETACHED"

// detachedEnabled is read on every Launch/Resume so an operator who
// flips ITERION_RUNS_DETACHED between runs gets the new behaviour
// without restarting the server.
func detachedEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envDetached))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// runnerCommand identifies which CLI subcommand the detached runner
// should invoke. Replacing a free-form string with a typed value
// keeps the buildRunnerCmd switch exhaustive at compile time.
type runnerCommand string

const (
	runnerCommandRun    runnerCommand = "run"
	runnerCommandResume runnerCommand = "resume"
)

// runnerBinary returns the absolute path to the iterion executable
// the server should spawn. We prefer os.Executable() (the running
// server's own binary) so a single iterion build is used end-to-end —
// any divergence between the editor and the runner would be a
// debugging nightmare. Falls back to PATH lookup when os.Executable
// is unavailable (rare).
func runnerBinary() (string, error) {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe, nil
	}
	return exec.LookPath("iterion")
}

// detachedSpec describes a single subprocess invocation. It is built
// from a LaunchSpec or ResumeSpec and turned into an os/exec command
// by buildRunnerCmd.
type detachedSpec struct {
	Command    runnerCommand
	RunID      string
	FilePath   string
	Vars       map[string]string // Launch only
	Answers    map[string]string // Resume only; CLI --answer accepts string values today
	StoreDir   string
	Force      bool
	Timeout    time.Duration
	MergeInto  string // worktree finalization, Launch only
	BranchName string // worktree finalization, Launch only
}

// buildRunnerCmd assembles the exec.Cmd for a detached-runner
// invocation. Returns the cmd ready to Start() — caller is
// responsible for SysProcAttr (Setsid).
func buildRunnerCmd(ctx context.Context, bin string, spec detachedSpec) (*exec.Cmd, error) {
	var args []string
	switch spec.Command {
	case runnerCommandRun:
		args = append(args, "run", spec.FilePath, "--background", "--run-id", spec.RunID, "--no-interactive")
		for k, v := range spec.Vars {
			args = append(args, "--var", k+"="+v)
		}
		if spec.MergeInto != "" {
			args = append(args, "--merge-into", spec.MergeInto)
		}
		if spec.BranchName != "" {
			args = append(args, "--branch-name", spec.BranchName)
		}
	case runnerCommandResume:
		args = append(args, "resume", "--background", "--no-interactive", "--run-id", spec.RunID, "--file", spec.FilePath)
		if spec.Force {
			args = append(args, "--force")
		}
		for k, v := range spec.Answers {
			args = append(args, "--answer", k+"="+v)
		}
	default:
		return nil, fmt.Errorf("runview: detached: unknown command %q", spec.Command)
	}
	if spec.StoreDir != "" {
		args = append(args, "--store-dir", spec.StoreDir)
	}
	if spec.Timeout > 0 {
		args = append(args, "--timeout", spec.Timeout.String())
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	// The runner's stdout/stderr go to /dev/null in production —
	// observability is via events.jsonl + run.log, not the runner's
	// stdio. Silencing them prevents the editor server's terminal from
	// being flooded by every spawned runner's logs.
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	return cmd, nil
}

// spawnDetached launches an iterion CLI subprocess for the given spec,
// detaches it from the server's session/process group, registers a
// runHandle with the manager, and starts a watcher goroutine that
// closes the handle's done channel when the subprocess exits.
//
// Returns once the runner has been Start'd (PID assigned) and the .pid
// file has been written. If anything fails before that point, the
// caller must clean up any partial state (the run lock, log buffer)
// just like the in-process spawnRun path.
func (s *Service) spawnDetached(parent context.Context, spec detachedSpec) (*LaunchResult, error) {
	bin, err := runnerBinary()
	if err != nil {
		return nil, fmt.Errorf("runview: detached: locate iterion binary: %w", err)
	}

	// Build the cmd against parent so a parent-cancel (SIGTERM during
	// Drain) marks the cmd as cancellable. Once Start succeeds the
	// runner is detached — parent cancellation does NOT kill it; the
	// SIGTERM-to-process-group path is owned by Manager.Cancel via
	// the registered closure.
	cmd, err := buildRunnerCmd(parent, bin, spec)
	if err != nil {
		return nil, err
	}

	// Detach from the server's process group so a server SIGTERM does
	// NOT propagate to the runner. Setsid creates a new session with
	// the runner as the leader — same call we'd use to daemonise.
	cmd.SysProcAttr = newDetachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runview: detached: start runner: %w", err)
	}
	pid := cmd.Process.Pid

	// Server-side .pid write closes the window between Start() and
	// the runner reaching its own deferred RemovePIDFile: a reconciler
	// query in that window would otherwise see no .pid and treat the
	// run as in-process.
	if pidS := store.AsPIDStore(s.store); pidS != nil {
		if err := pidS.WritePIDFile(spec.RunID, pid); err != nil {
			s.logger.Warn("runview: detached: write .pid for %s: %v", spec.RunID, err)
		}
	}

	done := make(chan struct{})
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			// Send SIGTERM to the process group so the runner
			// + any descendants (claude_code, codex, MCP servers)
			// terminate together.
			if err := terminateProcessGroup(pid); err != nil {
				s.logger.Warn("runview: detached: signal pgrp %d: %v", pid, err)
			}
		})
	}

	if err := s.manager.RegisterDetached(spec.RunID, pid, cancel, done); err != nil {
		// Manager already stopped — kill the runner we just started so
		// we don't leak it.
		_ = terminateProcessGroup(pid)
		return nil, err
	}

	// Watcher: cmd.Wait() blocks until the runner exits, after which
	// we tidy up the .pid file (in case the runner was SIGKILL'd
	// before its own deferred RemovePIDFile ran), drop subscribers,
	// and Deregister — the last step closes done.
	go func() {
		_ = cmd.Wait()
		if pidS := store.AsPIDStore(s.store); pidS != nil {
			_ = pidS.RemovePIDFile(spec.RunID)
		}
		s.broker.CloseRun(spec.RunID)
		s.dropRunLog(spec.RunID)
		s.manager.Deregister(spec.RunID)
	}()

	return &LaunchResult{RunID: spec.RunID, Done: done}, nil
}

// launchDetached is the Launch path that delegates to a spawned
// runner subprocess. It validates the .iter file (so an obviously bad
// workflow is rejected at the API boundary instead of silently
// failing inside the spawned process), starts the runner, and wires
// up the file-based event + log tailers so the editor's WS
// subscribers see the runner's output.
func (s *Service) launchDetached(parent context.Context, runID string, spec LaunchSpec) (*LaunchResult, error) {
	// Up-front compile so a malformed .iter doesn't get past the API
	// boundary. We discard the compiled IR — the runner subprocess
	// will compile its own copy from the same source. This duplicate
	// work is acceptable: it gives the caller a synchronous error for
	// the common case of a typo-laden workflow without forcing them to
	// poll events.jsonl for a compile failure.
	if _, _, err := CompileWorkflowWithHash(spec.FilePath); err != nil {
		return nil, err
	}

	// Set up the per-run log buffer so live WS subscribers can pick up
	// the file-based tailer's output. In detached mode we explicitly
	// skip the file-tee path: the runner subprocess owns run.log on
	// disk and a duplicate writer here would corrupt it.
	s.prepareRunLogNoFile(runID)

	res, err := s.spawnDetached(parent, detachedSpec{
		Command:    runnerCommandRun,
		RunID:      runID,
		FilePath:   spec.FilePath,
		Vars:       spec.Vars,
		StoreDir:   s.storeDir,
		Timeout:    spec.Timeout,
		MergeInto:  spec.MergeInto,
		BranchName: spec.BranchName,
	})
	if err != nil {
		s.dropRunLog(runID)
		return nil, err
	}

	// Start the file-based event tailer so subscribers to
	// broker.Subscribe(runID) see events the runner writes to
	// events.jsonl. It runs until the run terminates (signaled via
	// the closed Done channel from spawnDetached's watcher).
	startEventSource(s, runID, res.Done)
	startLogSource(s, runID, res.Done)

	return res, nil
}

// resumeDetached is the Resume counterpart to launchDetached.
func (s *Service) resumeDetached(parent context.Context, spec ResumeSpec) (*LaunchResult, error) {
	if _, _, err := CompileWorkflowWithHash(spec.FilePath); err != nil {
		return nil, err
	}

	s.prepareRunLogNoFile(spec.RunID)

	answers, convErr := resumeAnswersToStrings(spec.Answers)
	if convErr != nil {
		s.dropRunLog(spec.RunID)
		return nil, convErr
	}

	res, err := s.spawnDetached(parent, detachedSpec{
		Command:  runnerCommandResume,
		RunID:    spec.RunID,
		FilePath: spec.FilePath,
		Answers:  answers,
		StoreDir: s.storeDir,
		Force:    spec.Force,
		Timeout:  spec.Timeout,
	})
	if err != nil {
		s.dropRunLog(spec.RunID)
		return nil, err
	}

	startEventSource(s, spec.RunID, res.Done)
	startLogSource(s, spec.RunID, res.Done)

	return res, nil
}

// errProcessNotFound is returned by liveness probes when kill -0
// reports the target PID does not exist (ESRCH).
var errProcessNotFound = errors.New("runview: detached: process not found")

// pidAlive returns nil if the given PID is currently alive, or
// errProcessNotFound if the process no longer exists. Other errors
// (EPERM, etc.) are returned as-is so the reconciler can decide what
// to do — under typical operation the editor server has permission to
// signal its own children, but in unusual setups (rootless containers
// reparenting to PID 1) a "permission denied" answer effectively means
// "another process owns this; treat as alive".
func pidAlive(pid int) error {
	if pid <= 0 {
		return errProcessNotFound
	}
	return pidAliveOS(pid)
}

// resumeAnswersToStrings narrows the runtime resume API's
// map[string]interface{} into the string form the iterion CLI flag
// parser accepts. Non-string answer values are JSON-encoded so they
// round-trip through `--answer key=value` losslessly.
//
// Currently iterion `--answer` accepts only string values. For
// answers shaped like booleans, numbers, or objects we'd need a
// JSON-aware --answers-file path; this stub is sufficient for the
// common case of free-form text questions.
func resumeAnswersToStrings(answers map[string]interface{}) (map[string]string, error) {
	out := make(map[string]string, len(answers))
	for k, v := range answers {
		switch tv := v.(type) {
		case string:
			out[k] = tv
		case bool:
			out[k] = strconv.FormatBool(tv)
		case float64:
			out[k] = strconv.FormatFloat(tv, 'f', -1, 64)
		case int:
			out[k] = strconv.Itoa(tv)
		default:
			return nil, fmt.Errorf("runview: detached: unsupported answer type for key %q (%T) — non-scalar answers require --answers-file", k, v)
		}
	}
	return out, nil
}
