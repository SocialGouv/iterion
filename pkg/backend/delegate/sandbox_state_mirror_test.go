package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// copyBasedRun is a sandbox whose workspace is a COPY of the host's — the
// kubernetes shape. It implements WorkspaceFileRefresher, which is precisely
// how a caller learns that a host-side write will NOT be visible to the agent.
type copyBasedRun struct {
	*recordingRun
	written  map[string][]byte
	failWith error
}

func newCopyBasedRun(script string) *copyBasedRun {
	return &copyBasedRun{recordingRun: &recordingRun{script: script}, written: map[string][]byte{}}
}

func (r *copyBasedRun) Driver() string { return "kubernetes" }

func (r *copyBasedRun) RefreshWorkspaceFile(_ context.Context, relPath string, value []byte) error {
	if r.failWith != nil {
		return r.failWith
	}
	r.written[relPath] = append([]byte(nil), value...)
	return nil
}

// The regression this whole seam exists for, caught live on prod run 019fb968:
// the extension was written on the runner pod and `pi -e <host path>` ran in a
// sandbox pod that had never seen it, so every attempt died with "Extension
// path does not exist".
func TestMirrorStateFileIntoSandbox(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, ".iterion", "pi", "iterion-pi-1.js")

	t.Run("copy-based driver gets the file written through", func(t *testing.T) {
		run := newCopyBasedRun("true")
		task := Task{WorkDir: work, Sandbox: run}
		if err := mirrorStateFileIntoSandbox(context.Background(), task, target, []byte("BODY")); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		got, ok := run.written[filepath.Join(".iterion", "pi", "iterion-pi-1.js")]
		if !ok {
			t.Fatalf("nothing written into the sandbox; got %v", run.written)
		}
		if string(got) != "BODY" {
			t.Errorf("content = %q, want BODY", got)
		}
	})

	t.Run("a shared-filesystem driver is left alone", func(t *testing.T) {
		// docker bind mounts and the noop passthrough share the host inode, so
		// mirroring would be a redundant second write. They deliberately do not
		// implement the interface — the type assertion IS the oracle.
		task := Task{WorkDir: work, Sandbox: &recordingRun{script: "true"}}
		if err := mirrorStateFileIntoSandbox(context.Background(), task, target, []byte("BODY")); err != nil {
			t.Errorf("shared-filesystem driver must be a no-op, got %v", err)
		}
	})

	t.Run("hostless run is a no-op", func(t *testing.T) {
		if err := mirrorStateFileIntoSandbox(context.Background(), Task{WorkDir: work}, target, []byte("B")); err != nil {
			t.Errorf("no sandbox must be a no-op, got %v", err)
		}
	})

	t.Run("a path outside the workspace fails LOUDLY", func(t *testing.T) {
		// RefreshWorkspaceFile addresses the pod workspace root, so there is no
		// in-pod address for this file at all. Returning nil would report
		// success for something the agent can never read — the exact failure
		// this seam exists to end.
		run := newCopyBasedRun("true")
		outside := filepath.Join(t.TempDir(), "elsewhere.js")
		err := mirrorStateFileIntoSandbox(context.Background(), Task{WorkDir: work, Sandbox: run}, outside, []byte("B"))
		if err == nil {
			t.Fatal("expected an error for a path outside the workspace")
		}
		if len(run.written) != 0 {
			t.Errorf("nothing should have been written, got %v", run.written)
		}
	})

	t.Run("a write-through failure propagates", func(t *testing.T) {
		run := newCopyBasedRun("true")
		run.failWith = os.ErrPermission
		err := mirrorStateFileIntoSandbox(context.Background(), Task{WorkDir: work, Sandbox: run}, target, []byte("B"))
		if err == nil {
			t.Fatal("a failed write-through must not be swallowed")
		}
	})
}

// The WIRING, not the helper: a defect that lives in an uncalled seam is
// exactly what round 4 of the adversarial campaign found, so this drives a real
// CLIAgentBackend.Execute and asserts the system prompt reached the pod.
func TestSystemPromptFileReachesACopyBasedSandbox(t *testing.T) {
	work := t.TempDir()
	run := newCopyBasedRun("echo '{}'")

	b := &CLIAgentBackend{Protocol: piProtocol, Logger: testLogger()}
	task := Task{
		NodeID:       "n",
		WorkDir:      work,
		StoreDir:     "", // force the state root under the workspace
		Sandbox:      run,
		SystemPrompt: "OPERATING POSTURE",
		UserPrompt:   "do the thing",
		Model:        "openai/gpt-5.4-mini",
	}
	_, _ = b.Execute(context.Background(), task) // the fake CLI's output is irrelevant here

	var found string
	for rel, body := range run.written {
		if strings.HasSuffix(rel, ".sysprompt.md") {
			found = string(body)
		}
	}
	if found == "" {
		t.Fatalf("the system prompt never reached the sandbox; written = %v", keysOf(run.written))
	}
	if !strings.Contains(found, "OPERATING POSTURE") {
		t.Errorf("mirrored prompt = %q, want the composed posture", found)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ sandbox.WorkspaceFileRefresher = (*copyBasedRun)(nil)
