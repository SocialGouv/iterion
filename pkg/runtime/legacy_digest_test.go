package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestCheckWorkflowHash_AcceptsTheLegacyBareDigest: under the engine's own
// check, a run that recorded the bare digest of its bundle's main.bot from
// before the promotion is accepted — and marked so the artifacts it
// published under that revision are accepted with it — without the run
// being rewritten; a run that recorded neither digest is refused; a run
// with no bundle has no bare digest to match.
func TestCheckWorkflowHash_AcceptsTheLegacyBareDigest(t *testing.T) {
	dir := t.TempDir()
	mainBot := filepath.Join(dir, "main.bot")
	src := []byte("workflow w:\n  entry: done\n")
	if err := os.WriteFile(mainBot, src, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	bare := hex.EncodeToString(sum[:])
	promoted := "1111111111111111111111111111111111111111111111111111111111111111"

	e := &Engine{workflowHash: promoted, bundle: &bundle.Bundle{IterPath: mainBot}}
	r := &store.Run{ID: "run-legacy", WorkflowHash: bare}
	if err := e.checkWorkflowHash(r); err != nil {
		t.Fatalf("the legacy bare digest was refused: %v", err)
	}
	if !e.legacyDigestAccepted {
		t.Fatal("the acceptance was not recorded for the artifact revision waiver")
	}
	if r.WorkflowHash != bare {
		t.Fatalf("the run was rewritten to %s; the engine accepts, it does not rewrite", r.WorkflowHash)
	}

	other := &Engine{workflowHash: promoted, bundle: &bundle.Bundle{IterPath: mainBot}}
	if err := other.checkWorkflowHash(&store.Run{ID: "run-other", WorkflowHash: "2222222222222222222222222222222222222222222222222222222222222222"}); !errors.Is(err, ErrWorkflowSourceChanged) {
		t.Fatalf("a run that recorded neither digest: err = %v, want the source-changed refusal", err)
	}
	if other.legacyDigestAccepted {
		t.Fatal("a refused run marked the acceptance")
	}

	loose := &Engine{workflowHash: promoted}
	if err := loose.checkWorkflowHash(&store.Run{ID: "run-loose", WorkflowHash: bare}); !errors.Is(err, ErrWorkflowSourceChanged) {
		t.Fatalf("with no bundle the bare digest matched nothing to accept: err = %v", err)
	}
}
