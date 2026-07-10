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
