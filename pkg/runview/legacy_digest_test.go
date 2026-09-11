package runview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_MigratesTheLegacyBareDigest: a run launched from a bundle's
// main.bot before the bundle's prompts entered the workflow digest recorded
// the digest of the source bytes alone. Resumed after the promotion, it
// compares a different digest and would be refused as a source change that
// never happened — instead the preflight accepts the bare digest and the
// resume rewrites the run's to the bundle's, once, and goes on. A run that
// recorded neither stays refused, and a forced resume needs no migration.
func TestResume_MigratesTheLegacyBareDigest(t *testing.T) {
	dir := promptedBundle(t)
	opened, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, promoted, err := CompileBundleWorkflow(opened.IterPath, opened)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(opened.IterPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	bare := hex.EncodeToString(sum[:])
	if bare == promoted {
		t.Fatal("the bare and bundle digests coincide; the fixture ships no prompts")
	}

	storeDir := t.TempDir()
	st, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	save := func(id, digest string) {
		t.Helper()
		if err := st.SaveRun(ctx, &store.Run{
			FormatVersion: store.RunFormatVersion,
			ID:            id,
			WorkflowName:  "w",
			WorkflowHash:  digest,
			FilePath:      opened.IterPath,
			BundlePath:    dir,
			Status:        store.RunStatusFailedResumable,
		}); err != nil {
			t.Fatalf("save run: %v", err)
		}
	}
	save("run-legacy", bare)
	save("run-other", "0000000000000000000000000000000000000000000000000000000000000000")
	svc, err := NewService(storeDir, WithLogger(iterlog.Nop()), WithStore(st), WithLaunchPublisher(&stubLaunchPublisher{}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	legacy := ResumeSpec{RunID: "run-legacy", FilePath: opened.IterPath}
	if err := svc.PreflightResume(ctx, legacy); err != nil {
		t.Fatalf("preflight refused the legacy bare digest: %v", err)
	}
	if r, _ := st.LoadRun(ctx, "run-legacy"); r.WorkflowHash != bare {
		t.Fatalf("the preflight rewrote the digest (%s); only the resume migrates", r.WorkflowHash)
	}
	if _, err := svc.Resume(ctx, legacy); err != nil {
		t.Fatalf("resume refused the legacy bare digest: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if r.WorkflowHash != promoted {
		t.Fatalf("after the resume the run records %s, want the bundle's %s", r.WorkflowHash, promoted)
	}

	other := ResumeSpec{RunID: "run-other", FilePath: opened.IterPath}
	if err := svc.PreflightResume(ctx, other); !errors.Is(err, runtime.ErrWorkflowSourceChanged) {
		t.Fatalf("a run that recorded neither digest: preflight err = %v, want the source-changed refusal", err)
	}
	if _, err := svc.Resume(ctx, other); !errors.Is(err, runtime.ErrWorkflowSourceChanged) {
		t.Fatalf("a run that recorded neither digest: resume err = %v, want the source-changed refusal", err)
	}
	if r, _ := st.LoadRun(ctx, "run-other"); r.WorkflowHash == promoted {
		t.Fatal("a run that recorded neither digest was migrated")
	}

	// The helper itself: a loose file (no bundle) never matches, and a
	// digest that already is the bundle's is not a migration.
	if LegacyBareDigestMatches(&store.Run{WorkflowHash: bare}, nil) {
		t.Fatal("a nil bundle matched")
	}
	if MigrateLegacyBareDigest(ctx, st, &store.Run{ID: "x", WorkflowHash: promoted}, opened, promoted, nil) {
		t.Fatal("a run already on the bundle's digest was migrated")
	}
	_ = filepath.Join
}
