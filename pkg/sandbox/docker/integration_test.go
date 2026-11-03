//go:build dockerlive

// Package docker integration tests — gated by the `dockerlive` build
// tag because they require a working docker/podman daemon and pull
// the alpine:3 image (~3 MB). Run with:
//
//	devbox run -- go test -tags dockerlive ./pkg/sandbox/docker/...
//
// The unit tests in driver_test.go run without this tag and cover the
// pure-Go logic.
package docker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

const integrationImage = "alpine:3"

func newDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New()
	if err != nil {
		t.Skipf("docker driver unavailable: %v", err)
	}
	return d.(*Driver)
}

func tempWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iterion-sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestLiveLifecycle(t *testing.T) {
	d := newDriver(t)
	ws := tempWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	prepared, err := d.Prepare(ctx, sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Image: integrationImage,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	run, err := d.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         "live-test-" + time.Now().Format("150405"),
		WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(ctx)

	// 1. Exec captures stdout cleanly.
	res, err := run.Exec(ctx, []string{"sh", "-c", "echo hello-from-sandbox"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0; stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "hello-from-sandbox") {
		t.Errorf("stdout = %q, want to contain hello-from-sandbox", res.Stdout)
	}

	// 2. Workspace bind-mount is RW: write a file inside, read it back on host.
	res, err = run.Exec(ctx, []string{"sh", "-c", "echo from-inside > /workspace/marker"}, sandbox.ExecOpts{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("write inside: err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr)
	}
	host, err := os.ReadFile(filepath.Join(ws, "marker"))
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if strings.TrimSpace(string(host)) != "from-inside" {
		t.Errorf("host marker = %q, want from-inside", string(host))
	}

	// 3. Env vars passed via ExecOpts are visible.
	res, _ = run.Exec(ctx, []string{"sh", "-c", "echo $MY_VAR"}, sandbox.ExecOpts{
		Env: map[string]string{"MY_VAR": "world"},
	})
	if !strings.Contains(string(res.Stdout), "world") {
		t.Errorf("env-injected MY_VAR not visible: %q", res.Stdout)
	}

	// 4. Non-zero exit propagates.
	res, err = run.Exec(ctx, []string{"sh", "-c", "exit 17"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec exit-17: %v", err)
	}
	if res.ExitCode != 17 {
		t.Errorf("ExitCode = %d, want 17", res.ExitCode)
	}

	// 5. Command produces a Cmd that streams stdin.
	in := bytes.NewBufferString("piped-input\n")
	c := run.Command(ctx, []string{"cat"}, sandbox.ExecOpts{Stdin: in})
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		t.Fatalf("Cmd.Run: %v", err)
	}
	if !strings.Contains(out.String(), "piped-input") {
		t.Errorf("Cmd stdout = %q, want piped-input", out.String())
	}

	// 6. Cleanup removes the container.
	if err := run.Cleanup(ctx); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
	// Idempotent.
	if err := run.Cleanup(ctx); err != nil {
		t.Errorf("Cleanup (second): %v", err)
	}
}

func TestLivePostCreate(t *testing.T) {
	d := newDriver(t)
	ws := tempWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	prepared, err := d.Prepare(ctx, sandbox.Spec{
		Mode:       sandbox.ModeInline,
		Image:      integrationImage,
		PostCreate: "echo post-create-marker > /workspace/postcreate",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         "live-postcreate-" + time.Now().Format("150405"),
		WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(ctx)

	host, err := os.ReadFile(filepath.Join(ws, "postcreate"))
	if err != nil {
		t.Fatalf("postCreate did not write file: %v", err)
	}
	if !strings.Contains(string(host), "post-create-marker") {
		t.Errorf("file content = %q, want post-create-marker", string(host))
	}
}

func TestLivePostCreateFailureSurfacesError(t *testing.T) {
	d := newDriver(t)
	ws := tempWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prepared, err := d.Prepare(ctx, sandbox.Spec{
		Mode:       sandbox.ModeInline,
		Image:      integrationImage,
		PostCreate: "exit 99",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	_, err = d.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         "live-postcreate-fail-" + time.Now().Format("150405"),
		WorkspacePath: ws,
	})
	if err == nil {
		t.Fatal("expected Start to fail when postCreate exits non-zero")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error %q does not mention exit code 99", err.Error())
	}
}

// TestLiveExecEnvReachesTheInnerProcessOffTheArgv proves the GUARANTEE the
// unit witnesses pin the site of (#2845 item 2): against a real daemon, a
// per-exec value the argv never carries still arrives in the environment of
// the process inside the container — including the shapes the pair form
// could not carry at all (a multi-line value) and the one it carried as a
// deliberate override (the empty value, which overrides the image's `ENV`).
func TestLiveExecEnvReachesTheInnerProcessOffTheArgv(t *testing.T) {
	d := newDriver(t)
	ws := tempWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	prepared, err := d.Prepare(ctx, sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Image: integrationImage,
		Env:   map[string]string{"ITERION_LIVE_OVERRIDDEN": "from-the-container"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         "live-exec-env-" + time.Now().Format("150405"),
		WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(ctx)

	for _, tc := range []struct {
		name    string
		envName string
		value   string
	}{
		{"a provider token", "ITERION_LIVE_TOKEN", "sk-live-2845-0123456789abcdef"},
		{"a multi-line value", "ITERION_LIVE_PEM", "-----BEGIN PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END PRIVATE KEY-----"},
		{"the empty value overriding the container's own", "ITERION_LIVE_OVERRIDDEN", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := sandbox.ExecOpts{Env: map[string]string{tc.envName: tc.value}}
			// The same cmd answers both halves: its argv is what `ps`
			// would show on the host, and its output is what the process
			// inside the container read.
			cmd := run.Command(ctx, []string{"sh", "-c", `printf %s "${` + tc.envName + `?unset}"`}, opts)
			for _, arg := range cmd.Args {
				if strings.HasPrefix(arg, tc.envName+"=") {
					t.Errorf("the host argv carries the pair %q", arg)
				}
				if tc.value != "" && strings.Contains(arg, tc.value) {
					t.Errorf("the host argv carries the value inside %q", arg)
				}
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("exec: %v (stderr=%q)", err, stderr.String())
			}
			if got := stdout.String(); got != tc.value {
				t.Errorf("the process inside the container read %q, want %q", got, tc.value)
			}
		})
	}
}

// TestLiveExecEnvDoesNotReconfigureTheRuntimeClient proves the other half
// of the channel against a real client: the pair of a name-only `--env`
// travels in the CLIENT's environment, so an exec whose env names HOME —
// every claw node's does (`runnerEnv["HOME"]`, pkg/backend/model/claw_
// backend.go) — must not hand that HOME to the client.
//
// The exec's HOME here holds a `.docker/config.json` naming a context that
// does not exist: a client reading it fails to resolve its endpoint and
// never reaches a daemon, so the exec SUCCEEDING is the proof that the
// engine's own configuration is still the one in force — while the process
// inside the container reads the HOME the exec asked for.
func TestLiveExecEnvDoesNotReconfigureTheRuntimeClient(t *testing.T) {
	d := newDriver(t)
	ws := tempWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// A HOME whose client configuration is a dead end. The engine's own
	// HOME is untouched: only this exec's env names it.
	containerHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(containerHome, ".docker"), 0o755); err != nil {
		t.Fatalf("seeding the exec's HOME: %v", err)
	}
	if err := os.WriteFile(filepath.Join(containerHome, ".docker", "config.json"),
		[]byte(`{"currentContext":"iterion-live-2845-absent-context"}`), 0o600); err != nil {
		t.Fatalf("seeding the exec's HOME: %v", err)
	}

	prepared, err := d.Prepare(ctx, sandbox.Spec{Mode: sandbox.ModeInline, Image: integrationImage})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         "live-client-env-" + time.Now().Format("150405"),
		WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(ctx)

	const credential = "sk-live-2845-beside-the-home"
	cmd := run.Command(ctx, []string{"sh", "-c", `printf %s "$HOME"`}, sandbox.ExecOpts{
		Env: map[string]string{"HOME": containerHome, "ITERION_LIVE_TOKEN": credential},
	})
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("exec: %v — the client resolved its own configuration from the exec's HOME (stderr=%q)", err, stderr.String())
	}
	if got := stdout.String(); got != containerHome {
		t.Errorf("the process inside the container read HOME=%q, want the exec's own %q", got, containerHome)
	}
	// The credential beside it still travels off the argv.
	for _, arg := range cmd.Args {
		if strings.Contains(arg, credential) {
			t.Errorf("the host argv carries the credential inside %q", arg)
		}
	}
}
