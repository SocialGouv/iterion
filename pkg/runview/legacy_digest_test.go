package runview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_AcceptsTheLegacyBareDigest: a run launched from a bundle's
// main.bot before the bundle's prompts entered the workflow digest recorded
// the digest of the source bytes alone. Resumed after the promotion, it
// compares a different digest and would be refused as a source change that
// never happened — instead the preflight and the resume accept the bare
// digest and go on, without rewriting the run (a whole-document save
// outside the engine's claim would race its other writers). A run that
// recorded neither digest stays refused.
func TestResume_AcceptsTheLegacyBareDigest(t *testing.T) {
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
	if _, err := svc.Resume(ctx, legacy); err != nil {
		t.Fatalf("resume refused the legacy bare digest: %v", err)
	}
	if r, _ := st.LoadRun(ctx, "run-legacy"); r.WorkflowHash != bare {
		t.Fatalf("the run's digest was rewritten to %s outside the engine's claim", r.WorkflowHash)
	}

	other := ResumeSpec{RunID: "run-other", FilePath: opened.IterPath}
	if err := svc.PreflightResume(ctx, other); !errors.Is(err, runtime.ErrWorkflowSourceChanged) {
		t.Fatalf("a run that recorded neither digest: preflight err = %v, want the source-changed refusal", err)
	}
	if _, err := svc.Resume(ctx, other); !errors.Is(err, runtime.ErrWorkflowSourceChanged) {
		t.Fatalf("a run that recorded neither digest: resume err = %v, want the source-changed refusal", err)
	}
}
