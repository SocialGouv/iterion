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
}

// spawnProcess starts a new Claude CLI subprocess with the given arguments.
func spawnProcess(ctx context.Context, cliPath string, args []string, opts spawnOptions) (*cliProcess, error) {
	cmd := exec.CommandContext(ctx, cliPath, args...)

	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	// Build environment
	cmd.Env = os.Environ()
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stderr pipe: %w", err)
	}

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
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("claude: marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = p.stdin.Write(data)
	return err
}

// close shuts down the subprocess gracefully.
func (p *cliProcess) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	// Close stdin to signal the process to exit.
	_ = p.stdin.Close()
	return p.cmd.Wait()
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
