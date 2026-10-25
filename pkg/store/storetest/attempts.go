package storetest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/store"
)

// AttemptSnapshots exercises the same immutable blob contract in FS and
// Mongo/S3. Mutation: replace the historical body or truncate at the legacy
// 1 MiB best-effort cap; the old source and resource assertions fail.
func AttemptSnapshots(t *testing.T, st store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := st.CreateRun(ctx, "attempts", "w", nil); err != nil {
		t.Fatal(err)
	}
	snapshots := store.AsAttemptStore(st)
	if snapshots == nil {
		t.Fatal("store lacks AttemptStore")
	}
	original := strings.Repeat("x", 1024*1024+1)
	source := &store.AttemptSnapshot{StartedAt: time.Now().UTC(), WorkflowHash: "old", Budget: &store.RunBudget{MaxTokens: 100}, Source: &workflowfile.Snapshot{Main: "main.bot", Files: map[string]string{"main.bot": "workflow w:\n  entry: done\n", "prompts/large.md": original}}}
	first, err := snapshots.RecordAttempt(ctx, "attempts", source)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := snapshots.RecordAttempt(ctx, "attempts", source)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != first.ID {
		t.Fatal("identical snapshot is not idempotent")
	}
	source.Source.Files["prompts/large.md"] = "changed"
	source.WorkflowHash = "new"
	source.Budget.MaxTokens = 200
	second, err := snapshots.RecordAttempt(ctx, "attempts", source)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("different attempts share an ID")
	}
	old, err := snapshots.LoadAttempt(ctx, "attempts", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Source == nil || old.Budget == nil || old.Source.Files["prompts/large.md"] != original || old.Budget.MaxTokens != 100 {
		t.Fatal("history is overwritten or truncated")
	}
	current, err := snapshots.LoadAttempt(ctx, "attempts", second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, source) {
		t.Fatal("latest snapshot does not round-trip")
	}
	run, err := st.LoadRun(ctx, "attempts")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Attempts) != 2 || run.Attempts[0].ID != first.ID || run.Attempts[1].ID != second.ID {
		t.Fatalf("attempt index: %+v", run.Attempts)
	}
	if _, err := snapshots.LoadAttempt(ctx, "attempts", "../escape"); err == nil {
		t.Fatal("accepts traversal instead of digest")
	}
	if _, err := snapshots.LoadAttempt(ctx, "another-run", first.ID); err == nil {
		t.Fatal("snapshot is readable through another run")
	}
}
