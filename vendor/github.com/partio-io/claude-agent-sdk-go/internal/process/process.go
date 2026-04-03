package process

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Process manages a Claude CLI subprocess.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr io.ReadCloser

	mu     sync.Mutex
	closed bool

	stderrCallback func(string)
}

// Spawn starts a new Claude CLI subprocess with the given arguments.
func Spawn(ctx context.Context, cliPath string, args []string, opts SpawnOptions) (*Process, error) {
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

	p := &Process{
		cmd:            cmd,
		stdin:          stdin,
		stdout:         scanner,
		stderr:         stderr,
		stderrCallback: opts.StderrCallback,
	}

	// Drain stderr in a background goroutine. It exits when the subprocess's
	// stderr pipe is closed (i.e., the process exits or Close is called).
	go p.drainStderr()

	return p, nil
}

// SpawnOptions configures subprocess spawning.
type SpawnOptions struct {
	Cwd            string
	Env            map[string]string
	StderrCallback func(string)
}

// ReadLine reads the next NDJSON line from stdout.
// Returns io.EOF when the process has no more output.
func (p *Process) ReadLine() ([]byte, error) {
	if p.stdout.Scan() {
		return p.stdout.Bytes(), nil
	}
	if err := p.stdout.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// WriteLine writes a JSON value as NDJSON to stdin.
func (p *Process) WriteLine(v any) error {
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

// Close shuts down the subprocess gracefully.
func (p *Process) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	// Close stdin to signal the process to exit.
	_ = p.stdin.Close() // best-effort; process exit is what matters
	return p.cmd.Wait()
}

// Wait waits for the subprocess to exit and returns any error.
func (p *Process) Wait() error {
	return p.cmd.Wait()
}

// ExitCode returns the exit code after the process has exited.
// Returns -1 if the process hasn't exited yet.
func (p *Process) ExitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

func (p *Process) drainStderr() {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if p.stderrCallback != nil {
			p.stderrCallback(line)
		}
	}
}
