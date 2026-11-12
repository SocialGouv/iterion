package model

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

const transportEnvProbe = "ITERION_TEST_DRIVER_TRANSPORT"

// The workload map and host transport are deliberately separate. Only the
// driver's Cmd carries the nonce; replacing its environment with os.Environ
// loses it even when the workload values still reach the sandbox correctly.
type transportProbeRun struct {
	sandbox.Run
	seen    []sandbox.ExecOpts
	capture string
	host    bool
}

func (r *transportProbeRun) HostExecutesCommands() bool { return r.host }

func (*transportProbeRun) Driver() string { return "transport-probe" }
func (r *transportProbeRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	if isNodeSessionEnd(argv) {
		return exec.CommandContext(ctx, "true")
	}
	opts.Env = maps.Clone(opts.Env)
	r.seen = append(r.seen, opts)
	script := `printf '%s' "$ITERION_TEST_DRIVER_TRANSPORT"`
	if r.capture != "" {
		script += ` > "$1"`
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "transport-probe", r.capture)
	cmd.Env = append(os.Environ(), transportEnvProbe+"=driver-only")
	return cmd
}

func unsetTransportProbe(t *testing.T) {
	t.Helper()
	t.Setenv(transportEnvProbe, "")
	if err := os.Unsetenv(transportEnvProbe); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxToolConstructorsPreserveDriverTransport(t *testing.T) {
	for _, host := range []bool{false, true} {
		t.Run(fmt.Sprintf("host-%t", host), func(t *testing.T) {
			unsetTransportProbe(t)
			e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(t.TempDir()))
			run := &transportProbeRun{host: host}
			e.SetSandbox(run)
			commands := []toolCommand{
				e.toolNodeCommand(context.Background(), "true", map[string]string{"WORKLOAD": "carried"}),
				e.toolNodeScriptCommand(context.Background(), []string{"sh"}, "true", map[string]string{"WORKLOAD": "carried"}),
			}
			for i, command := range commands {
				output, err := command.cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				if string(output) != "driver-only" {
					t.Errorf("TRANSPORT_LOST: constructor %d produced %q", i, output)
				}
				if run.seen[i].Env["WORKLOAD"] != "carried" {
					t.Errorf("constructor %d lost workload environment", i)
				}
				if _, exists := run.seen[i].Env[transportEnvProbe]; exists {
					t.Errorf("constructor %d moved host transport into the workload map", i)
				}
			}
		})
	}
}

func TestClawRunnerPreservesDriverTransport(t *testing.T) {
	unsetTransportProbe(t)
	operatorEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-transport-fixture")
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "0")
	run := &transportProbeRun{capture: filepath.Join(t.TempDir(), "transport")}
	// The local fixture exits without speaking the runner protocol. The
	// dispatch may report EOF; the assertion is the environment of the real
	// process the complete runner crossing actually starts, not its response.
	_, _ = newTestClawBackend().executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "transport", Model: "openai/gpt-6", WorkDir: t.TempDir(), Sandbox: run,
	})
	output, err := os.ReadFile(run.capture)
	if err != nil {
		t.Fatal("runner did not start the transport probe:", err)
	}
	if string(output) != "driver-only" {
		t.Fatalf("TRANSPORT_LOST: runner produced %q", output)
	}
	if len(run.seen) != 1 || !run.seen[0].KeepStdinOpen {
		t.Fatalf("runner invocation did not retain its streaming options: %d", len(run.seen))
	}
	if _, exists := run.seen[0].Env[transportEnvProbe]; exists {
		t.Fatal("runner moved host transport into the workload map")
	}
}
