package claudesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// processConfig holds parameters that map to CLI flags.
type processConfig struct {
	Model              string
	SystemPrompt       string
	AppendSystemPrompt string
	Cwd                string
	Verbose            bool

	AllowedTools    []string
	DisallowedTools []string
	PermissionMode  string

	MaxTurns     int
	MaxBudgetUSD float64

	IncludePartialMessages bool

	Resume               string
	ForkSession          bool
	ContinueConversation bool
	NoSessionPersistence bool

	OutputFormat map[string]any
	AddDirs      []string

	// MCPConfigJSON is the JSON-encoded MCP server config (written to a temp file).
	MCPConfigJSON []byte

	// AgentsJSON is the JSON-encoded agents config.
	AgentsJSON []byte
}

// buildArgs converts a processConfig into CLI arguments for `claude --print`.
func buildArgs(cfg processConfig, streaming bool) []string {
	args := []string{"--print"}

	if streaming {
		args = append(args, "--output-format", "stream-json")
		args = append(args, "--input-format", "stream-json")
	} else {
		args = append(args, "--output-format", "stream-json")
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
	}
	if cfg.Verbose {
		args = append(args, "--verbose")
	}
	if cfg.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}

	if len(cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	}
	if len(cfg.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(cfg.DisallowedTools, ","))
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}

	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", cfg.MaxTurns))
	}
	if cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%g", cfg.MaxBudgetUSD))
	}

	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	if cfg.ForkSession {
		args = append(args, "--fork-session")
	}
	if cfg.ContinueConversation {
		args = append(args, "--continue")
	}
	if cfg.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}

	if cfg.OutputFormat != nil {
		if b, err := json.Marshal(cfg.OutputFormat); err == nil {
			args = append(args, "--json-schema", string(b))
		}
	}

	for _, dir := range cfg.AddDirs {
		args = append(args, "--add-dir", dir)
	}

	if len(cfg.AgentsJSON) > 0 {
		args = append(args, "--agents", string(cfg.AgentsJSON))
	}

	return args
}

// buildMCPConfigJSON creates the JSON for --mcp-config from a map of server configs.
func buildMCPConfigJSON(servers map[string]any) ([]byte, error) {
	cfg := map[string]any{"mcpServers": servers}
	return json.Marshal(cfg)
}

// findCLI locates the claude binary. It checks:
// 1. Explicit path (if provided)
// 2. PATH lookup
// 3. ~/.claude/local/claude
func findCLI(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", &cliNotFoundError{searched: []string{explicit}}
	}

	// Check PATH
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}

	// Check ~/.claude/local/
	home, err := os.UserHomeDir()
	if err == nil {
		local := filepath.Join(home, ".claude", "local", "claude")
		if _, err := os.Stat(local); err == nil {
			return local, nil
		}
	}

	return "", &cliNotFoundError{
		searched: []string{"PATH", "~/.claude/local/claude"},
	}
}

// cliProcess manages a Claude CLI subprocess.
type cliProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr io.ReadCloser

	mu     sync.Mutex
	closed bool

	stderrCallback func(string)
}

// spawnOptions configures subprocess spawning.
type spawnOptions struct {
	Cwd            string
	Env            map[string]string
	StderrCallback func(string)
	// CommandBuilder, when non-nil, replaces the default
	// exec.CommandContext invocation with the caller's constructor.
	// Iterion's sandbox driver uses this to route the claude CLI
	// through `docker exec` — see [CommandBuilder] in option.go.
	//
	// When set, the SDK does NOT apply Cwd/Env to the returned cmd
	// directly: the builder is responsible for passing them through
	// the runtime-native channels (e.g. `--workdir` / `--env` flags
	// to `docker exec`).
	CommandBuilder CommandBuilder
	// OpenStdin opens a writable pipe to the child stdin (required by the
	// streaming Session: NDJSON messages are written there). When false,
	// cmd.Stdin is left nil so Go attaches /dev/null to the child stdin —
	// this avoids the CLI's "no stdin data received in 3s" warning that
	// fires when stdin is a non-TTY pipe with no data and the input format
	// is not stream-json (the one-shot Prompt() case, where the prompt is
	// passed as a CLI argument).
	OpenStdin bool
}

// spawnProcess starts a new Claude CLI subprocess with the given arguments.
func spawnProcess(ctx context.Context, cliPath string, args []string, opts spawnOptions) (*cliProcess, error) {
	var cmd *exec.Cmd
	if opts.CommandBuilder != nil {
		// Sandbox-routed path: the builder owns Cwd/Env propagation
		// (typically via --workdir / --env flags to `docker exec`),
		// so the SDK does not apply them to the returned cmd.
		cmd = opts.CommandBuilder(ctx, cliPath, args, opts.Cwd, opts.Env)
		if cmd == nil {
			return nil, fmt.Errorf("claude: CommandBuilder returned nil cmd")
		}
	} else {
		cmd = exec.CommandContext(ctx, cliPath, args...)
		if opts.Cwd != "" {
			cmd.Dir = opts.Cwd
		}
		cmd.Env = os.Environ()
		for k, v := range opts.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	var stdin io.WriteCloser
	if opts.OpenStdin {
		var err error
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("claude: stdin pipe: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stderr pipe: %w", err)
	}

	// Make the subprocess the leader of its own process group so we can
	// reliably terminate the entire subtree (MCP servers, bash background
	// jobs spawned by `run_in_background` / Monitor) on close. Without this
	// a hung subtree blocks cmd.Wait() indefinitely.
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	// 10 MB buffer for large NDJSON lines.
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	p := &cliProcess{
		cmd:            cmd,
		stdin:          stdin,
		stdout:         scanner,
		stderr:         stderr,
		stderrCallback: opts.StderrCallback,
	}

	// Drain stderr in a background goroutine.
	go p.drainStderr()

	return p, nil
}

// readLine reads the next NDJSON line from stdout.
// Returns io.EOF when the process has no more output.
func (p *cliProcess) readLine() ([]byte, error) {
	if p.stdout.Scan() {
		return p.stdout.Bytes(), nil
	}
	if err := p.stdout.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// writeLine writes a JSON value as NDJSON to stdin.
func (p *cliProcess) writeLine(v any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("claude: write to closed process")
	}
	if p.stdin == nil {
		return fmt.Errorf("claude: stdin not opened (one-shot mode)")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("claude: marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = p.stdin.Write(data)
	return err
}

// Defaults for the close-time termination ladder. Overridable via env for
// operators who need to tune the budget without rebuilding.
//
//   - closeGraceTimeout: window after stdin EOF for the subprocess to exit
//     on its own. The CLI normally exits within milliseconds; we allow a
//     few seconds for slow shutdown paths (final stdout flush, MCP
//     teardown).
//   - closeTermTimeout: how long we wait after SIGTERM before escalating
//     to SIGKILL. Short — SIGTERM is best-effort; we don't block runs on
//     well-behaved shutdown.
const (
	defaultCloseGraceTimeout = 3 * time.Second
	defaultCloseTermTimeout  = 1 * time.Second
)

func resolveDuration(envName string, fallback time.Duration) time.Duration {
	if v := os.Getenv(envName); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// close shuts down the subprocess. The contract is that close() must
// return in bounded time even when the child or its descendants are
// hung — otherwise the caller's `defer sess.Close()` deadlocks the
// surrounding workflow (we hit this with the Claude CLI keeping bash
// background loops alive after the agent had logically finished).
//
// The shutdown ladder:
//  1. Close stdin to signal a graceful exit.
//  2. Wait up to closeGraceTimeout for the child to exit on its own.
//  3. Send SIGTERM to the whole process group, wait up to closeTermTimeout.
//  4. Send SIGKILL to the whole process group and block on Wait — at this
//     point the kernel guarantees a fast collection.
//
// We always block on cmd.Wait() somewhere so cmd.ProcessState is populated
// and the OS reaps the child. If Wait surfaces a kill-induced exit error,
// we discard it: a forced shutdown is the expected outcome on this path,
// not a failure to report upstream.
func (p *cliProcess) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	graceTimeout := resolveDuration("ITERION_CLAUDE_CODE_CLOSE_GRACE", defaultCloseGraceTimeout)
	termTimeout := resolveDuration("ITERION_CLAUDE_CODE_CLOSE_TERM", defaultCloseTermTimeout)

	waitDone := make(chan error, 1)
	go func() { waitDone <- p.cmd.Wait() }()

	pid := 0
	if p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}

	// Phase 1: graceful exit on stdin EOF.
	if graceTimeout > 0 {
		select {
		case err := <-waitDone:
			return err
		case <-time.After(graceTimeout):
		}
	}

	// Phase 2: SIGTERM the whole subtree, give it a moment to unwind.
	if pid > 0 {
		_ = killProcessGroup(pid, syscall.SIGTERM)
	}
	if termTimeout > 0 {
		select {
		case err := <-waitDone:
			return ignoreSignalExit(err)
		case <-time.After(termTimeout):
		}
	}

	// Phase 3: SIGKILL — non-negotiable. Wait blocks until the kernel
	// reaps the child, which is now imminent.
	if pid > 0 {
		_ = killProcessGroup(pid, syscall.SIGKILL)
	}
	return ignoreSignalExit(<-waitDone)
}

// ignoreSignalExit discards "exit status N" / "signal: killed" errors that
// stem from our own forced shutdown. A real error path (e.g. ENOENT at
// startup, broken pipe) still produces a non-ExitError that we propagate.
func ignoreSignalExit(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return err
}

// exitCode returns the exit code after the process has exited.
// Returns -1 if the process hasn't exited yet.
func (p *cliProcess) exitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

func (p *cliProcess) drainStderr() {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if p.stderrCallback != nil {
			p.stderrCallback(line)
		}
	}
}
