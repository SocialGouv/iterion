package operatormcp

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/store"
)

// errProcessNotFound is returned by pidAlive when the probed PID no
// longer exists.
var errProcessNotFound = errors.New("operatormcp: process not found")

// runnerCommand identifies which CLI subcommand a detached runner
// invokes (the operatormcp counterpart of runview's detachedSpec).
type runnerCommand string

const (
	runnerCommandRun    runnerCommand = "run"
	runnerCommandResume runnerCommand = "resume"
)

// runnerSpec describes one detached `iterion run|resume --background`
// invocation.
type runnerSpec struct {
	Command  runnerCommand
	RunID    string
	FilePath string
	StoreDir string

	// Launch only.
	Vars                map[string]string
	Timeout             string
	MergeInto           string
	BranchName          string
	MaxCostUSD          float64
	MaxTokens           int
	MaxDuration         string
	MaxIterations       int
	MaxParallelBranches int

	// Resume only.
	Answers map[string]string
	Force   bool
}

// buildRunnerArgs assembles the CLI argv for a runner spec. Map-valued
// flags are emitted in sorted key order so the argv is deterministic.
func buildRunnerArgs(spec runnerSpec) ([]string, error) {
	var args []string
	switch spec.Command {
	case runnerCommandRun:
		args = append(args, "run", spec.FilePath, "--background", "--run-id", spec.RunID, "--no-interactive")
		for _, k := range sortedKeys(spec.Vars) {
			args = append(args, "--var", k+"="+spec.Vars[k])
		}
		if spec.Timeout != "" {
			args = append(args, "--timeout", spec.Timeout)
		}
		if spec.MergeInto != "" {
			args = append(args, "--merge-into", spec.MergeInto)
		}
		if spec.BranchName != "" {
			args = append(args, "--branch-name", spec.BranchName)
		}
		if spec.MaxCostUSD > 0 {
			args = append(args, "--max-cost-usd", strconv.FormatFloat(spec.MaxCostUSD, 'f', -1, 64))
		}
		if spec.MaxTokens > 0 {
			args = append(args, "--max-tokens", strconv.Itoa(spec.MaxTokens))
		}
		if spec.MaxDuration != "" {
			args = append(args, "--max-duration", spec.MaxDuration)
		}
		if spec.MaxIterations > 0 {
			args = append(args, "--max-iterations", strconv.Itoa(spec.MaxIterations))
		}
		if spec.MaxParallelBranches > 0 {
			args = append(args, "--max-parallel-branches", strconv.Itoa(spec.MaxParallelBranches))
		}
	case runnerCommandResume:
		args = append(args, "resume", "--background", "--no-interactive", "--run-id", spec.RunID, "--file", spec.FilePath)
		if spec.Force {
			args = append(args, "--force")
		}
		for _, k := range sortedKeys(spec.Answers) {
			args = append(args, "--answer", k+"="+spec.Answers[k])
		}
	default:
		return nil, fmt.Errorf("operatormcp: unknown runner command %q", spec.Command)
	}
	if spec.StoreDir != "" {
		args = append(args, "--store-dir", spec.StoreDir)
	}
	return args, nil
}

// runnerBinary resolves the iterion CLI binary to spawn. Same
// resolution as runview's detached mode: a stable installed binary
// first (LocateIterionBinary skips volatile go-run build paths), PATH
// as the last resort.
func runnerBinary() (string, error) {
	if p := proc.LocateIterionBinary(); p != "" {
		return p, nil
	}
	return exec.LookPath("iterion")
}

// spawnDetachedRunner starts the runner subprocess in its own session
// (so it survives this MCP server's exit), writes the run's .pid file,
// and reaps the child in the background. Returns the runner PID.
func spawnDetachedRunner(st *store.FilesystemRunStore, spec runnerSpec) (int, error) {
	bin, err := runnerBinary()
	if err != nil {
		return 0, fmt.Errorf("locate iterion binary: %w", err)
	}
	args, err := buildRunnerArgs(spec)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(bin, args...)
	// Observability is events.jsonl + run.log, not the runner's stdio —
	// and stdio MUST stay closed: an inherited stdout would let the
	// runner corrupt this server's JSON-RPC stream.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid

	// Spawner-side .pid write, mirroring the studio server: the
	// --background runner only removes the file on exit, it never
	// writes it.
	if pidS := store.AsPIDStore(st); pidS != nil {
		if err := pidS.WritePIDFile(spec.RunID, pid); err != nil {
			// The run is already started; a missing .pid only degrades
			// cancel/liveness, so report it in-band rather than killing
			// the run.
			go reapRunner(cmd, nil, spec.RunID)
			return pid, fmt.Errorf("run %s started (pid %d) but writing its .pid failed: %w", spec.RunID, pid, err)
		}
		go reapRunner(cmd, pidS, spec.RunID)
		return pid, nil
	}
	go reapRunner(cmd, nil, spec.RunID)
	return pid, nil
}

// reapRunner waits on the child so it never lingers as a zombie while
// this MCP server lives, and tidies the .pid file in case the runner
// was killed before its own deferred cleanup ran.
func reapRunner(cmd *exec.Cmd, pidS store.PIDStore, runID string) {
	_ = cmd.Wait()
	if pidS != nil {
		_ = pidS.RemovePIDFile(runID)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
