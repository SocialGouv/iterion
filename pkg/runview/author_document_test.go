package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An author document (.bot.yaml) is a draft of a .bot, never a program: the
// compile floor under every entry refuses it by name — never as a parse
// error of a text that was YAML — and Launch refuses it before the pipeline
// queue can persist a run for it (the queue records a run before compiling).
func TestAnAuthorDocumentIsRefusedBeforeAnyRunExists(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "draft.bot.yaml")
	if err := os.WriteFile(draft, []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CompileWorkflow(draft); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("CompileWorkflow: %v, want ErrAuthorDocument", err)
	}
	if _, _, err := CompileWorkflowWithHash(draft); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("CompileWorkflowWithHash: %v, want ErrAuthorDocument", err)
	}
	if _, _, err := CompileWorkflowFromSource(draft, "dsl: 2\n"); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("CompileWorkflowFromSource: %v, want ErrAuthorDocument", err)
	}

	storeDir := filepath.Join(dir, ".iterion")
	svc, err := NewService(storeDir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: draft}); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("Launch: %v, want ErrAuthorDocument", err)
	}
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: "draft.bot.yaml", Source: "dsl: 2\n"}); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("Launch with an inline source named as a draft: %v, want ErrAuthorDocument", err)
	}
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("a refused launch left run docs behind: %v", ids)
	}
}

// Launch refuses a draft on its own, BEFORE the pipeline queue: over the
// concurrency cap the queue persists a queued run doc without compiling it,
// so the compile floor never sees the draft — a witness without a queue
// proves the floor, not this door. The draft is spelled in capitals: every
// door asks the one case-folded predicate.
func TestAnAuthorDocumentIsNeverQueued(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "DRAFT.BOT.YAML")
	if err := os.WriteFile(draft, []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, ".iterion")
	svc, err := NewService(storeDir, WithLogger(iterlog.Nop()), WithMaxConcurrentPipelines(1))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	t.Cleanup(func() { svc.StopBackground(ctx) })
	if svc.pipelineQueue == nil {
		t.Fatal("no pipeline queue: the test would prove the compile floor, not the door before the queue")
	}
	// The one slot is held by a root that never frees it: the next root
	// launch is parked, not compiled.
	if admitted, _ := svc.pipelineQueue.admitOrEnqueue("run-occupant", LaunchSpec{FilePath: "occupant.bot"}); !admitted {
		t.Fatal("the occupant was not admitted: the queue is not saturated")
	}
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: draft}); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("Launch over the cap: %v, want ErrAuthorDocument", err)
	}
	if qs := svc.pipelineQueue.status(); qs.Waiting != 0 {
		t.Fatalf("the draft was queued: %d waiting", qs.Waiting)
	}
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("a refused launch persisted a queued run: %v", ids)
	}
}
