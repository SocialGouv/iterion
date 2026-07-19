package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A DSL-supplied mount_path is tenant-controlled, and the no-sandbox runner
// writes file secrets to the host pod filesystem. An out-of-tree mount_path
// (e.g. /root/.ssh/authorized_keys) must be refused so a crafted workflow
// cannot write a secret value to an arbitrary host path.
func TestMaterializeFileSecretsNoSandboxRejectsOutOfTreeMountPath(t *testing.T) {
	evil := filepath.Join(t.TempDir(), "authorized_keys") // absolute, outside /run/iterion/secrets

	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelError, os.Stderr)}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{"evil": "PWNED"},
	})
	wf := &ir.Workflow{
		Secrets: map[string]*ir.Secret{
			"evil": {Name: "evil", As: "file", MountPath: evil},
		},
		// Sandbox nil → the no-sandbox materialize path runs.
	}

	written, cleanup, err := r.materializeFileSecretsNoSandbox(ctx, wf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup != nil || len(written) != 0 {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("expected nothing written, got %v", written)
	}
	if _, statErr := os.Stat(evil); !os.IsNotExist(statErr) {
		t.Fatalf("out-of-tree secret file was written despite the containment guard: %s (stat err: %v)", evil, statErr)
	}
}

// TestMaterializeFileSecretsNoSandbox_GateShapes pins the no-op gates in
// front of the host-side materialisation: no workflow, no file secrets, an
// active resolved sandbox (which mounts secrets into the container
// instead), and a file secret with no resolved credential value all yield
// (nil, nil, nil) — nothing touches the pod filesystem.
func TestMaterializeFileSecretsNoSandbox_GateShapes(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelError, os.Stderr)}}
	credCtx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{"forge_token": "tok"},
	})

	cases := []struct {
		name string
		ctx  context.Context
		wf   *ir.Workflow
	}{
		{"nil workflow", credCtx, nil},
		{"no secrets", credCtx, &ir.Workflow{}},
		{"env-only secret", credCtx, &ir.Workflow{Secrets: map[string]*ir.Secret{
			"forge_token": {Name: "forge_token", As: "env"},
		}}},
		// The RESOLVED sandbox decision gates — a workflow-declared active
		// sandbox mounts file secrets into the container, so the host-side
		// materialisation must not run even though the value resolves.
		{"active sandbox", credCtx, &ir.Workflow{
			Sandbox: &ir.SandboxSpec{Mode: "auto"},
			Secrets: map[string]*ir.Secret{"forge_token": {Name: "forge_token", As: "file"}},
		}},
		{"unresolved value skipped", context.Background(), &ir.Workflow{Secrets: map[string]*ir.Secret{
			"forge_token": {Name: "forge_token", As: "file"},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			written, cleanup, err := r.materializeFileSecretsNoSandbox(c.ctx, c.wf)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if written != nil || cleanup != nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatalf("expected nothing written, got %v", written)
			}
		})
	}
}

// TestRemoveFilesFunc pins the cleanup closure: existing files are removed
// and already-missing paths are tolerated (idempotent, never panics).
func TestRemoveFilesFunc(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	rm := removeFilesFunc([]string{a, b, filepath.Join(dir, "never-existed")})
	rm()
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived removeFilesFunc", p)
		}
	}
	rm() // second call is a no-op
}
