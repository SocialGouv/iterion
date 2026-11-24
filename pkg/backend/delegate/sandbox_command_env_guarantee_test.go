package delegate

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

const delegateDriverNonceKey = "ITERION_TEST_DELEGATE_DRIVER_NONCE"
const delegateWorkloadKey = "ITERION_TEST_DELEGATE_WORKLOAD"

// This fake models two distinct channels. Cmd.Env belongs to the host driver;
// ExecOpts.Env travels as arguments to the local container stand-in. Never run
// the real wrapper/sweep scripts on the host: they can signal other processes.
// Every assertion below reads a file written by the real child, not Cmd.Env.
type envGuaranteeRun struct {
	t                *testing.T
	dir, nonce, mode string
	mu               sync.Mutex
	calls            []envGuaranteeCall
}
type envGuaranteeCall struct {
	path    string
	argv    []string
	open    bool
	cleanup bool
}

func newEnvGuaranteeRun(t *testing.T, mode string) *envGuaranteeRun {
	t.Helper()
	for _, key := range []string{delegateDriverNonceKey, delegateWorkloadKey} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s must be absent from ambient environment", key)
		}
	}
	return &envGuaranteeRun{t: t, dir: t.TempDir(), nonce: rand.Text(), mode: mode}
}
func (*envGuaranteeRun) Driver() string                { return "test-env-transport" }
func (*envGuaranteeRun) Cleanup(context.Context) error { return nil }
func (r *envGuaranteeRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	r.mu.Lock()
	cleanup := len(argv) == 3 && argv[0] == "sh" && strings.Contains(argv[2], "kill -TERM")
	path := filepath.Join(r.dir, fmt.Sprintf("child-%d", len(r.calls)))
	r.calls = append(r.calls, envGuaranteeCall{path, append([]string(nil), argv...), opts.KeepStdinOpen, cleanup})
	r.mu.Unlock()
	mode := r.mode
	if cleanup {
		mode = "cleanup"
	}
	script := `printf '%s\n%s\n%s\n' "$ITERION_TEST_DELEGATE_DRIVER_NONCE" "$ITERION_TEST_DELEGATE_WORKLOAD" "$2" > "$1"
case "$3" in
claude) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{}","session_id":"env-test"}'; cat > "$1.stdin" ;;
stream) cat > "$1.stdin"; printf 'completed\n' ;;
cleanup) : ;;
esac`
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "env-transport", path, opts.Env[delegateWorkloadKey], mode)
	cmd.Env = append(os.Environ(), delegateDriverNonceKey+"="+r.nonce)
	cmd.Stdin = opts.Stdin
	return cmd
}
func (r *envGuaranteeRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	cmd := r.Command(ctx, argv, opts)
	out, err := cmd.Output()
	return sandbox.ExecResult{Stdout: out}, err
}
func (r *envGuaranteeRun) assertChildren(t *testing.T, workload, stdin string, wantOpen, wantCleanup bool) {
	t.Helper()
	r.mu.Lock()
	calls := append([]envGuaranteeCall(nil), r.calls...)
	r.mu.Unlock()
	spawns, cleanups := 0, 0
	for _, call := range calls {
		body, err := os.ReadFile(call.path)
		if err != nil {
			t.Fatalf("child receipt: %v", err)
		}
		lines := strings.Split(string(body), "\n")
		expected := workload
		if call.cleanup {
			cleanups++
			expected = ""
		} else {
			spawns++
		}
		if len(lines) != 4 || lines[0] != r.nonce || lines[1] != "" || lines[2] != expected {
			t.Errorf("child lost driver environment or mixed channels: got %q; want nonce=%q, host workload empty, remote workload=%q", body, r.nonce, expected)
		}
		if !call.cleanup {
			if call.open != wantOpen {
				t.Errorf("KeepStdinOpen=%v, want %v", call.open, wantOpen)
			}
			if stdin != "" {
				data, err := os.ReadFile(call.path + ".stdin")
				if err != nil || !strings.Contains(string(data), stdin) {
					t.Errorf("child stdin=%q err=%v, want %q", data, err, stdin)
				}
			}
		}
	}
	if spawns != 1 || (wantCleanup && cleanups != 1) || (!wantCleanup && cleanups != 0) {
		t.Fatalf("spawn/cleanup counts=%d/%d", spawns, cleanups)
	}
}

func TestDelegateCommandPreservesDriverEnvironmentClaude(t *testing.T) {
	for _, mode := range []string{"prompt", "session", "format", "format-hooked"} {
		t.Run(mode, func(t *testing.T) {
			resetClaudeCredEnv(t)
			r := newEnvGuaranteeRun(t, "claude")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			task := Task{NodeID: "env-guarantee", Sandbox: r, ExtraEnv: []string{delegateWorkloadKey + "=remote-workload"}, OutputSchema: []byte(`{"type":"object"}`)}
			b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
			if mode == "format-hooked" {
				task.Permission = mustPolicy(t, permission.ModeDeny)
			}
			if strings.HasPrefix(mode, "format") {
				if _, _, _, err := b.formatOutput(ctx, task, "env-test"); err != nil {
					t.Fatalf("formatOutput: %v", err)
				}
			} else {
				opts, cleanup := b.buildSpawnOptions(task)
				if mode == "session" {
					s := claudesdk.NewSession(opts...)
					t.Cleanup(func() { _ = s.Close() })
					if err := s.Send(ctx, "driver-session-input"); err != nil {
						t.Fatal(err)
					}
					result := false
					for msg, err := range s.Stream(ctx) {
						if err != nil {
							t.Fatal(err)
						}
						if _, ok := msg.(*claudesdk.ResultMessage); ok {
							result = true
							break
						}
					}
					if !result {
						t.Fatal("session missing result")
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
				} else if _, err := claudesdk.Prompt(ctx, "driver-prompt-input", opts...); err != nil {
					t.Fatal(err)
				}
				cleanup()
			}
			stdin := ""
			if mode == "session" {
				stdin = "driver-session-input"
			}
			if mode == "format-hooked" {
				stdin = "Format your complete findings"
			}
			r.assertChildren(t, "remote-workload", stdin, mode == "session" || mode == "format-hooked", true)
		})
	}
}

func TestDelegateCommandPreservesDriverEnvironmentCLI(t *testing.T) {
	for _, stdin := range []string{"", "driver-cli-input"} {
		t.Run(fmt.Sprintf("stdin-%t", stdin != ""), func(t *testing.T) {
			r := newEnvGuaranteeRun(t, "stream")
			b := &CLIAgentBackend{Protocol: CLIAgentProtocol{Name: "testcli", ResolveEnv: func(context.Context) map[string]string { return map[string]string{delegateWorkloadKey: "protocol"} }}, Logger: iterlog.Nop()}
			task := Task{NodeID: "env-guarantee", Sandbox: r, ExtraEnv: []string{delegateWorkloadKey + "=remote-workload"}}
			out, _, _, err := b.runOnce(context.Background(), task, "local-fixture", []string{"--flag"}, stdin, nil, 10*time.Second)
			if err != nil || !strings.Contains(out, "completed") {
				t.Fatalf("runOnce output=%q err=%v", out, err)
			}
			r.assertChildren(t, "remote-workload", stdin, false, true)
		})
	}
}

func TestDelegateCommandPreservesDriverEnvironmentPi(t *testing.T) {
	r := newEnvGuaranteeRun(t, "stream")
	b := &PiRPCBackend{Logger: iterlog.Nop()}
	task := Task{NodeID: "env-guarantee", Sandbox: r, ExtraEnv: []string{delegateWorkloadKey + "=remote-workload"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := b.spawner(task, "local-pi", "local-mark", "")(ctx, []string{"--mode", "rpc"})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("driver-pi-input\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "completed") {
		t.Fatal("Pi child did not complete")
	}
	r.assertChildren(t, "remote-workload", "driver-pi-input", true, false)
}

func TestDelegateCommandPreservesDriverEnvironmentCleanup(t *testing.T) {
	r := newEnvGuaranteeRun(t, "cleanup")
	killSandboxDelegateReporting(r, "test-env", sweepOwner{logger: iterlog.Nop()})()
	r.mu.Lock()
	calls := append([]envGuaranteeCall(nil), r.calls...)
	r.mu.Unlock()
	if len(calls) != 1 || !calls[0].cleanup {
		t.Fatalf("cleanup calls=%v", calls)
	}
	body, err := os.ReadFile(calls[0].path)
	if err != nil || string(body) != r.nonce+"\n\n\n" {
		t.Fatalf("cleanup child lost driver environment: receipt=%q err=%v", body, err)
	}
}
