package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestCredParity_DelegateDispatch drives every row of ADR-161's parity table
// whose dispatch lives in this package (credparity.DispatchHarness) through
// the backend's real Execute — the one DefaultRegistry registers — and judges
// what the dispatch spends against the row:
//
//   - a typed refusal (credroute.Refusal) is the row's reason: the resolver
//     refused the leg before any binary, overlay or spawn;
//   - a dispatch that passes the resolver's gate spends what its backend
//     reads: an own-config CLI its configuration (ADR-161 decision 5), pi and
//     codex the pod's environment their spawn inherits on the host — the
//     leak the interim guard closes on a stamped run (decision 9).
//
// The CLI is staged on PATH and run through the noop driver (a host
// dispatch), whose fake container answers at once: no real CLI runs.
func TestCredParity_DelegateDispatch(t *testing.T) {
	// A dispatch that passes the gate prices its result: from the static
	// table, never a live fetch.
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Cleanup(modelspecs.SetDefault(modelspecs.New(modelspecs.Options{
		CachePath: filepath.Join(t.TempDir(), "modelspecs-absent.json"), NoAutoFetch: true,
	})))
	judged := 0
	for _, row := range credparity.Rows() {
		if credparity.DispatchHarness(row.Node.Backend) != credparity.DelegateHarness {
			continue // its dispatch is driven where its backend lives
		}
		judged++
		// pi has two transports, each its own way to the spawn: both are
		// dispatched, so the gate each one passes is witnessed alone.
		modes := []string{""}
		if row.Node.Backend == BackendPi {
			modes = []string{"print", "rpc"}
		}
		for _, mode := range modes {
			label := row.Label()
			if mode != "" {
				label += "_" + mode
			}
			t.Run(label, func(t *testing.T) { delegateParityRow(t, row, mode) })
		}
	}
	if judged == 0 {
		t.Fatal("inert harness: no row of the table dispatches a delegate backend")
	}
}

// delegateParityRow dispatches row's node through its backend's Execute (pi
// on the transport mode names) and judges what it spends.
func delegateParityRow(t *testing.T, row credparity.Row, mode string) {
	t.Helper()
	if row.Sandboxed || row.Role != credparity.NodeRole {
		t.Fatalf("the harness dispatches a node on the host; row %q is sandboxed=%v role %s", row.Name, row.Sandboxed, row.Role)
	}
	isolateUserHome(t)
	for _, name := range credparity.PostureNames {
		t.Setenv(name, "")
	}
	for name, v := range row.Posture {
		t.Setenv(name, v)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ITERION_PI_MODE", mode)
	dumps := stageRecordingCLI(t, row.Node.Backend)
	ctx := secrets.WithCredentials(context.Background(), row.Run.Credentials(""))
	backend, err := DefaultRegistry(testLogger()).Resolve(row.Node.Backend)
	if err != nil {
		t.Fatal(err)
	}
	run := &recordingRun{driver: "noop", script: "exit 0"}
	work := t.TempDir()
	_, execErr := backend.Execute(ctx, Task{
		NodeID: row.Node.ID, UserPrompt: "hi", Model: row.Node.Model,
		ProviderHint: row.Node.Hint(0), Sandbox: run, WorkDir: work, BaseDir: work,
	})
	credparity.Check(t, row, credparity.Dispatch, delegateParitySpend(t, row, execErr, run, dumps), func(s credparity.Spend) credparity.Spend { return s })
}

// stageRecordingCLI puts an executable named name first on PATH that records
// the environment of every invocation into its own file under the returned
// dir — the probe a dispatch runs before its spawn included — answers a
// `--version` with a version every gate accepts, and exits 0.
func stageRecordingCLI(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	bin, dumps := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nenv > \"" + dumps + "/$$.env\"\ncase \"$1\" in --version) echo \"" + name + " 9.9.9\";; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dumps
}

// delegateParitySpend names what a delegate dispatch spent: the resolver's
// typed refusal, else the channel its backend reads past the gate — its own
// config, or the pod credential its CLI receives: the env the noop driver
// hands the spawn on top of the launcher's, and the env every invocation of
// the staged CLI recorded (dumps).
func delegateParitySpend(t *testing.T, row credparity.Row, execErr error, run *recordingRun, dumps string) credparity.Spend {
	t.Helper()
	var refusal *credroute.Refusal
	if errors.As(execErr, &refusal) {
		if !errors.Is(execErr, credroute.ErrAmbientRefused) {
			t.Errorf("the %s refusal (%s) does not unwrap to credroute.ErrAmbientRefused: %v", row.Node.Backend, refusal.Reason, execErr)
		}
		if recorded := recordedEnvs(t, dumps); len(run.allArgv) > 0 || len(recorded) > 0 {
			t.Errorf("the dispatch ran its CLI (%v, %d invocation(s)) before its refusal", run.allArgv, len(recorded))
		}
		return credparity.Refuses(credparity.Reason(refusal.Reason))
	}
	recorded := recordedEnvs(t, dumps)
	if len(run.allArgv) == 0 && len(recorded) == 0 {
		t.Fatalf("the %s dispatch neither refused nor ran its CLI: %v", row.Node.Backend, execErr)
	}
	if ownConfigBackend(row.Node.Backend) {
		// The CLI ran: it spends what its own configuration holds.
		return credparity.Spent(0, credparity.OwnConfig, "", "")
	}
	inherited := map[string]string{}
	for _, opts := range run.allOpts {
		for k, v := range opts.Env {
			inherited[k] = v
		}
	}
	for name, v := range row.Posture {
		if got, ok := inherited[name]; len(run.allArgv) > 0 && (ok && got == v || !ok && os.Getenv(name) == v) {
			return credparity.Spent(0, credparity.PodEnv, "", v)
		}
		for _, env := range recorded {
			if env[name] == v {
				return credparity.Spent(0, credparity.PodEnv, "", v)
			}
		}
	}
	t.Fatalf("the %s dispatch ran past the resolver's gate (%v) spending nothing the harness can name", row.Node.Backend, execErr)
	return credparity.Spend{}
}

// recordedEnvs reads the environments the staged CLI recorded.
func recordedEnvs(t *testing.T, dumps string) []map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dumps, "*.env"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]string, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f) // #nosec G304 — a file this test's CLI wrote under its temp dir
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				env[k] = v
			}
		}
		out = append(out, env)
	}
	return out
}

// ownConfigBackend reports a backend whose arm answers its own config.
func ownConfigBackend(backend string) bool {
	for _, b := range ownConfigBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// The guard's refusal reads as the operator needs it at the dispatch: the
// backend, the arm-pending reason, the stamp — and it unwraps to the leaf's
// stamp sentinel like the own-config refusal, so every reader matching the
// stamp's refusal matches both.
func TestTheGuardsRefusalNamesItsBackendAndUnwrapsToTheStamp(t *testing.T) {
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	for _, backend := range armPendingBackends {
		_, err := credroute.Resolve(ctx, credroute.Call{Backend: backend, Spec: "anthropic/claude-sonnet-4", Role: credroute.RoleNode})
		var refusal *credroute.Refusal
		if !errors.As(err, &refusal) || refusal.Reason != credroute.ReasonArmPending {
			t.Fatalf("%s on a stamped run = %v, want the arm-pending refusal", backend, err)
		}
		if !errors.Is(err, credroute.ErrAmbientRefused) {
			t.Errorf("%s: the guard's refusal does not unwrap to credroute.ErrAmbientRefused", backend)
		}
		for _, needle := range []string{backend, string(credroute.ReasonArmPending), "ADR-161 U4"} {
			if !strings.Contains(err.Error(), needle) {
				t.Errorf("%s: refusal %q does not name %q", backend, err, needle)
			}
		}
		// Unstamped, the guard decides nothing: the readers keep their path.
		if _, err := credroute.Resolve(context.Background(), credroute.Call{Backend: backend, Spec: "anthropic/claude-sonnet-4"}); !errors.Is(err, credroute.ErrNoArm) {
			t.Errorf("%s unstamped = %v, want ErrNoArm — no arm decides it before U4", backend, err)
		}
	}
	for _, backend := range ownConfigBackends {
		if _, err := credroute.Resolve(ctx, credroute.Call{Backend: backend, Spec: "x/y"}); !errors.Is(err, credroute.ErrAmbientRefused) {
			t.Errorf("%s on a stamped run = %v, want the ambient-stamp refusal", backend, err)
		}
		spend, err := credroute.Resolve(context.Background(), credroute.Call{Backend: backend, Spec: "x/y"})
		if err != nil || spend.Channel != credroute.ChannelOwnConfig {
			t.Errorf("%s unstamped = %+v, %v — want the own-config channel", backend, spend, err)
		}
	}
}

// opencode asks the resolver before its workspace screen: under the stamp the
// leg's refusal is the stamp's, whatever the workspace holds — a workspace
// the screen would refuse included — so the operator reads the funding
// refusal, not a workspace one that a trusted project would lift for nothing.
func TestAStampedOpencodeLegIsRefusedBeforeItsWorkspaceScreen(t *testing.T) {
	stamped := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	backend, err := DefaultRegistry(testLogger()).Resolve(BackendOpenCode)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Execute(stamped, Task{NodeID: "impl", UserPrompt: "hi", Model: "anthropic/claude-sonnet-4"})
	var refusal *credroute.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != credroute.ReasonAmbientStamp {
		t.Fatalf("a stamped opencode leg with no workspace = %v, want the stamp's refusal before the workspace screen", err)
	}
	if _, err := backend.Execute(context.Background(), Task{NodeID: "impl", UserPrompt: "hi", Model: "anthropic/claude-sonnet-4"}); errors.As(err, &refusal) || err == nil {
		t.Fatalf("bench: an unstamped opencode leg with no workspace = %v, want the workspace screen's refusal", err)
	}
}
