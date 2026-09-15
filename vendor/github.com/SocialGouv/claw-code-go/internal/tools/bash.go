package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/permissions"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBashTimeout bounds ONE bash call. Short on purpose: most of an
	// agent's shell calls are probes, and a wedged probe costs the whole turn.
	DefaultBashTimeout = 30 * time.Second
	// maxBashTimeoutSeconds caps what a CALLER may ask for through the tool's
	// `timeout_seconds` input. The deployment's own CLAW_BASH_TIMEOUT is not
	// bounded by it: an operator setting a machine-wide default is trusted,
	// the model asking for a longer deadline mid-turn is not.
	maxBashTimeoutSeconds = 600
	maxOutputSize         = 10000
)

// bashTimeout resolves the bound on one bash call. It reads CLAW_BASH_TIMEOUT
// (a Go duration like "15m", or a bare number of seconds); 0 or negative leaves
// the caller's context as the only bound; an unset or unparsable value falls
// back to DefaultBashTimeout. Same shape as sseutil.StreamIdleTimeout, so the
// two duration knobs read alike.
//
// The default is a PROBE's budget, and that is the right default. It is the
// wrong budget for the call that matters most in a self-verifying loop: an
// agent asked to check its own work runs the repo's build and test suite,
// which is minutes on a large repo. With no way out, such an agent cannot
// verify what it changed — it can only claim to have, which is the failure the
// deterministic gate exists to prevent.
func bashTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("CLAW_BASH_TIMEOUT"))
	if v == "" {
		return DefaultBashTimeout
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		// Both sides: a large NEGATIVE n wraps back to a positive duration —
		// measured at 512ns, which refuses `echo hello` instantly while
		// pointing the reader at the knob.
		if int64(n) > int64(math.MaxInt64/time.Second) || int64(n) < int64(math.MinInt64/time.Second) {
			return DefaultBashTimeout
		}
		return time.Duration(n) * time.Second
	}
	return DefaultBashTimeout
}

// bashWarnWriter is the writer for bash validation warnings.
// Defaults to os.Stderr; tests can replace it to capture output.
var bashWarnWriter io.Writer

func bashStderr() io.Writer {
	if bashWarnWriter != nil {
		return bashWarnWriter
	}
	return os.Stderr
}

// bashOutput keeps the first maxOutputSize bytes and discards the rest as it
// arrives, so a command's whole output never lives in memory at once: the
// bound holds WHILE the command runs, not only once it returns. A build that
// prints hundreds of megabytes therefore costs a fixed buffer, for as long as
// its timeout allows it to keep printing.
//
// Stdout and Stderr are set to this same pointer on purpose — os/exec gives
// both streams one descriptor when the two writers are interface-equal, which
// is what serializes the writes instead of racing two goroutines on the array.
type bashOutput struct {
	data      [maxOutputSize]byte
	size      int
	truncated bool
}

func (b *bashOutput) Write(p []byte) (int, error) {
	n := copy(b.data[b.size:], p)
	b.size += n
	b.truncated = b.truncated || n < len(p)
	// Report the full length: a short write would make os/exec's copy loop
	// report io.ErrShortWrite instead of letting the command run on.
	return len(p), nil
}

func (b *bashOutput) String() string {
	output := string(b.data[:b.size])
	if b.truncated {
		output += "\n... [output truncated]"
	}
	return output
}

// BashTool returns the tool definition for the bash tool.
func BashTool() api.Tool {
	return api.Tool{
		Name: "bash",
		Description: "Execute a bash command and return its combined output. Use it to run builds, tests, git, and other programs.\n\n" +
			"Do NOT use bash to read or edit files when a dedicated tool fits: use read_file instead of cat/head/tail, the grep tool instead of shell grep, glob instead of find, and file_edit/write_file instead of sed/awk/echo redirection — shell text-mangling is error-prone and harder to review. " +
			"Chain dependent commands with && so a failure stops the sequence, and quote paths containing spaces. " +
			"Never run destructive operations (rm -rf, force-push, hard reset, history rewrites) unless the user explicitly asked for that exact operation.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"command": {
					Type:        "string",
					Description: "The bash command to execute",
				},
				"timeout_seconds": {
					Type:        "integer",
					Description: "Optional command deadline in whole seconds (1 to 600, default 30). Request a longer bounded deadline for builds or tests. Caller cancellation or an earlier caller deadline always wins; background processes are not a timeout workaround.",
				},
			},
			Required: []string{"command"},
		},
	}
}

// ExecuteBash runs a bash command and returns combined stdout+stderr.
// It validates the command against the current permission mode and workspace
// path before execution. Pass permissions.ModeAllow and "" to skip validation.
//
// callerCtx is the caller's context — cancelling it (e.g. from an outer
// run-cancel signal) aborts the bash process group promptly. Pass
// context.Background() if no upstream cancellation is needed.
//
// The spawned bash inherits os.Environ() of the calling process. Use
// ExecuteBashWithEnv when the caller needs to surface a project-managed
// toolchain (devbox, nix, asdf) whose bin path is not in the parent
// shell's PATH.
func ExecuteBash(callerCtx context.Context, input map[string]any, mode permissions.PermissionMode, workspace string) (string, error) {
	return ExecuteBashWithEnv(callerCtx, input, mode, workspace, nil)
}

// ExecuteBashWithEnv runs a bash command with extra environment
// variables appended to the inherited process environment. Each
// extraEnv entry uses the standard "KEY=value" format; later entries
// for the same key win (Go's exec.Cmd convention).
//
// The use case driving this entry point: when iterion is launched
// without the project's devbox/nix/asdf toolchain in PATH, the bash
// tool can't find go/gofmt/etc. and the LLM-driven fixer can't run
// `go test` to validate its patch. Passing devbox's bin path via
// extraEnv from the iterion side restores autonomy without forcing
// every operator to remember `devbox run --` at launch.
//
// Pass nil to inherit only the parent process's environment (same
// behaviour as the legacy ExecuteBash entry point).
//
// bash and all its descendants run in a dedicated process group so the
// timeout (or callerCtx cancellation) reaches grandchildren too. Without
// this, a `bash -c "node …"` whose node child spawned workers would
// orphan those workers to init on SIGKILL, and they'd keep the stdout
// pipe open — wedging cmd.Wait() forever.
func ExecuteBashWithEnv(callerCtx context.Context, input map[string]any, mode permissions.PermissionMode, workspace string, extraEnv []string) (string, error) {
	command, ok := input["command"].(string)
	if !ok || command == "" {
		return "", fmt.Errorf("bash: 'command' input is required and must be a string")
	}
	timeout, err := bashCommandTimeout(input)
	if err != nil {
		return "", err
	}

	// Validate command before execution.
	if workspace == "" {
		workspace = "."
	}
	result := ValidateCommand(command, mode, workspace)
	switch result.Kind {
	case ValidationBlock:
		return "", fmt.Errorf("bash: command blocked: %s", result.Reason)
	case ValidationWarn:
		// Log warning but proceed.
		fmt.Fprintf(bashStderr(), "bash warning: %s\n", result.Message)
	}

	if callerCtx == nil {
		callerCtx = context.Background()
	}
	start := time.Now()
	_, explicitTimeout := input["timeout_seconds"]
	// The caller's own `timeout_seconds` when it asked for one (bounded 1 to
	// maxBashTimeoutSeconds by bashCommandTimeout), otherwise the deployment
	// default CLAW_BASH_TIMEOUT resolves. Two dials, one budget: the per-call
	// one is the model asking for room on a build or a test suite, the env one
	// is the operator setting the floor everything else runs at.
	limit := timeout
	// A non-positive budget means EXACTLY what it says: this function installs
	// no bound, and the caller's context becomes the only one. It is reachable
	// only through the env dial — an explicit timeout_seconds is at least 1.
	// Read the consequence before setting it: with a caller context that never
	// cancels (the Background this function documents as its no-cancellation
	// case) a wedged command never returns at all, and its process group is
	// never reaped. os/exec runs the kill(-pgid) only from the goroutine it
	// starts while ctx.Done() is non-nil, and only if that Done fires BEFORE
	// Wait collects the result — so there is no arrangement here that reaps a
	// tree nothing ever cancelled. 0 is an escape hatch for a caller that owns
	// its own deadline, not a longer timeout.
	ctx, cancel := callerCtx, context.CancelFunc(func() {})
	if limit > 0 {
		ctx, cancel = context.WithTimeout(callerCtx, limit)
	}
	defer cancel()
	// Frozen at the gesture, not read at the conclusion: cmd.Run can spend up
	// to WaitDelay after the kill, so a caller deadline landing inside that
	// window would otherwise be blamed for a timeout the budget decided.
	knobDecided := false
	if limit > 0 {
		callerDL, ok := callerCtx.Deadline()
		knobDecided = !ok || !callerDL.Before(start.Add(limit))
	}

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	// Put bash and every descendant in a fresh process group so a single
	// signal reaches the whole tree (default exec.CommandContext only
	// signals the immediate child). Unix-only — Windows uses a stub
	// (see bash_windows.go). The build-tagged bash_unix.go installs
	// SysProcAttr.Setpgid + cmd.Cancel; cmd.WaitDelay below still
	// applies on every OS as the pipe-close safety net.
	applyBashProcessGroup(cmd)
	// Safety net: if a descendant somehow survives the group kill and
	// keeps stdout/stderr open, Go force-closes the pipes after
	// WaitDelay so cmd.Wait — and the io.Copy goroutines feeding buf —
	// unblock instead of hanging the entire runner.
	cmd.WaitDelay = 2 * time.Second

	var buf bashOutput
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err = cmd.Run()

	output := buf.String()

	if err != nil {
		// Return output + error description; the caller decides if it's a hard error
		if callerErr := callerCtx.Err(); callerErr != nil {
			return output, fmt.Errorf("command stopped by caller: %w", callerErr)
		}
		if ctx.Err() == context.DeadlineExceeded {
			// Name the dial that actually decided, of the three that can:
			// the caller's own timeout_seconds, the deployment default, or a
			// deadline neither of them set. Pointing at the wrong one sends
			// the reader to a knob that would not have changed the outcome.
			//
			// Every branch WRAPS ctx.Err(): a caller telling a deadline from
			// any other failure should not have to match on prose.
			if knobDecided {
				if explicitTimeout {
					return output, fmt.Errorf("command timed out after %s (raise it with timeout_seconds, up to %ds): %w", limit, maxBashTimeoutSeconds, ctx.Err())
				}
				return output, fmt.Errorf("command timed out after %s (raise it with timeout_seconds, or with CLAW_BASH_TIMEOUT for every call): %w", limit, ctx.Err())
			}
			return output, fmt.Errorf("command timed out on the caller's deadline: %w", ctx.Err())
		}
		if ctx.Err() == context.Canceled {
			return output, fmt.Errorf("command cancelled: %w", ctx.Err())
		}
		// For non-zero exit codes, return output with error appended
		return output, fmt.Errorf("command exited with error: %v", err)
	}

	return output, nil
}

// Validate before converting to Duration (which can overflow) or spawning any
// process. JSON-decoded tool inputs use float64; public Go callers may supply
// int/int64 or json.Number. Missing preserves the historical 30-second limit;
// null, non-numbers, fractions and out-of-range values are errors, not defaults.
func bashCommandTimeout(input map[string]any) (time.Duration, error) {
	raw, exists := input["timeout_seconds"]
	if !exists {
		// No per-call ask: the deployment's own dial decides, which may be
		// non-positive and therefore install no bound at all. That branch is
		// unreachable through this input — an explicit value is at least 1.
		return bashTimeout(), nil
	}
	invalid := fmt.Errorf("bash: 'timeout_seconds' must be an integer from 1 to %d", maxBashTimeoutSeconds)
	var seconds float64
	switch n := raw.(type) {
	case int:
		seconds = float64(n)
	case int64:
		seconds = float64(n)
	case float64:
		seconds = n
	case json.Number:
		if !json.Valid([]byte(n)) {
			return 0, invalid
		}
		var err error
		seconds, err = n.Float64()
		if err != nil {
			return 0, invalid
		}
	default:
		return 0, invalid
	}
	if math.IsNaN(seconds) || seconds < 1 || seconds > maxBashTimeoutSeconds || math.Trunc(seconds) != seconds {
		return 0, invalid
	}
	if n, ok := raw.(json.Number); ok {
		// UseNumber callers retained the lexeme. Do not round a fractional
		// value (e.g. 600.00000000000000001) into an allowed integer. Check
		// the range above first so huge exponents never reach the exact parser.
		rational, ok := new(big.Rat).SetString(string(n))
		if !ok || !rational.IsInt() {
			return 0, invalid
		}
	}
	return time.Duration(seconds) * time.Second, nil
}
