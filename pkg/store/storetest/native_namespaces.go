package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RunNativeNamespaces exercises the same native lifecycle through both
// durable backends. A missing required seam is a failure, never a skip.
func RunNativeNamespaces(t *testing.T, factory Factory) {
	t.Run("BlobClosure", func(t *testing.T) {
		s := factory(t)
		ctx := store.WithRuntimeSemantics(testCtx(), store.RuntimeSemanticsPortsV1)
		const id = "pc1_blobs"
		if _, err := s.CreateRun(ctx, id, "test", nil); err != nil {
			t.Fatal(err)
		}
		files := store.AsRunFilesStore(s)
		if files == nil {
			t.Fatal("missing files seam")
		}
		dir, err := files.EnsureRunFilesDir(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.ToSlash(dir)
		if !strings.Contains(path, "/"+store.NativeRunsDirectory+"/"+id) && !strings.Contains(path, "/ports-v1/"+id) {
			t.Fatalf("native scratch escapes its namespace: %s", path)
		}
		if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("report"), 0600); err != nil {
			t.Fatal(err)
		}
		if uploader := store.AsRunFilesUploader(s); uploader != nil {
			if n, err := uploader.UploadRunFiles(ctx, id); err != nil || n != 1 {
				t.Fatalf("upload: %d, %v", n, err)
			}
		}
		body, info, err := files.OpenRunFile(ctx, id, "report.txt")
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil || string(data) != "report" || info.Path != "report.txt" {
			t.Fatalf("file: %q, %+v, %v, %v", data, info, readErr, closeErr)
		}
		tools := store.AsToolBlobStore(s)
		if tools == nil {
			t.Fatal("missing tool blob seam")
		}
		if _, err := tools.WriteToolBlob(ctx, id, "call", "output", []byte("tool result")); err != nil {
			t.Fatal(err)
		}
		if data, _, _, err := tools.ReadToolBlob(ctx, id, "call", "output", 0, 0); err != nil || string(data) != "tool result" {
			t.Fatalf("tool blob: %q, %v", data, err)
		}
		sessions := store.AsBackendSessionStore(s)
		if sessions == nil {
			t.Fatal("missing backend sessions seam")
		}
		if err := sessions.PutBackendSession(ctx, id, "session", []byte("conversation")); err != nil {
			t.Fatal(err)
		}
		if data, err := sessions.GetBackendSession(ctx, id, "session"); err != nil || string(data) != "conversation" {
			t.Fatalf("session: %q, %v", data, err)
		}
		logs := store.AsRunLogStore(s)
		if logs == nil {
			t.Fatal("missing logs seam")
		}
		if err := logs.AppendRunLog(ctx, id, 0, []byte("log")); err != nil {
			t.Fatal(err)
		}
		if data, err := logs.ReadRunLogRange(ctx, id, 0, 0); err != nil || string(data) != "log" {
			t.Fatalf("log: %q, %v", data, err)
		}
		rec := store.AttachmentRecord{Name: "input", OriginalFilename: "input.txt", MIME: "text/plain"}
		if err := s.WriteAttachment(ctx, id, rec, bytes.NewReader([]byte("attachment"))); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.GetBackendSession(ctx, id, "session"); err == nil {
			t.Fatal("native session survived deletion")
		}
		if _, _, _, err := tools.ReadToolBlob(ctx, id, "call", "output", 0, 0); err == nil {
			t.Fatal("native tool blob survived deletion")
		}
		if body, _, err := files.OpenRunFile(ctx, id, "report.txt"); err == nil {
			body.Close()
			t.Fatal("native file survived deletion")
		}
	})
	t.Run("ExplicitIdentity", func(t *testing.T) {
		s := factory(t)
		legacy := testCtx()
		native := store.WithRuntimeSemantics(legacy, store.RuntimeSemanticsPortsV1)
		for _, tc := range []struct {
			ctx context.Context
			id  string
		}{
			{legacy, "pc1_missing_optin"}, {native, "legacy_id"},
			{native, "pc1_"}, {native, "pc2_future"},
			{store.WithRuntimeSemantics(legacy, "unknown"), "pc1_unknown"},
		} {
			if _, err := s.CreateRun(tc.ctx, tc.id, "test", nil); !errors.Is(err, store.ErrRunSemantics) {
				t.Errorf("create %s: %v", tc.id, err)
			}
		}
		if ids, err := s.ListRuns(legacy); err != nil || len(ids) != 0 {
			t.Fatalf("refused creations left runs: %v, %v", ids, err)
		}
		r, err := s.CreateRun(native, "pc1_identity", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.RuntimeSemantics != store.RuntimeSemanticsPortsV1 || r.FormatVersion != store.NativeRunFormatVersion {
			t.Fatalf("identity: %+v", r)
		}
		r.RuntimeSemantics = store.RuntimeSemanticsLegacyAdapterV1
		if err := s.SaveRun(native, r); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("changed interpreter: %v", err)
		}
		r.RuntimeSemantics = ""
		if err := s.SaveRun(native, r); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("semantic downgrade: %v", err)
		}
		r.RuntimeSemantics = store.RuntimeSemanticsPortsV1
		r.ID = "pc1_missing"
		r.CASVersion = 0
		if err := s.SaveRun(native, r); !errors.Is(err, store.ErrRunNotFound) {
			t.Fatalf("implicit native upsert: %v", err)
		}
	})
	t.Run("ExactPublicInputs", func(t *testing.T) {
		s := factory(t)
		ctx := store.WithRuntimeSemantics(testCtx(), store.RuntimeSemanticsPortsV1)
		want := map[string]any{"large": json.Number("9007199254740993"), "null": nil, "empty": []any{}, "nested": map[string]any{"fraction": json.Number("1.234567890123456789")}}
		if _, err := s.CreateRun(ctx, "pc1_values", "test", want); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			r, err := s.LoadRun(ctx, "pc1_values")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.Inputs, want) {
				t.Fatalf("public values changed: got %#v; want %#v", r.Inputs, want)
			}
			r.Name = "renamed"
			if err := s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("DescendantNamespace", func(t *testing.T) {
		s := factory(t)
		ctx := store.WithRuntimeSemantics(testCtx(), store.RuntimeSemanticsPortsV1)
		if _, err := s.CreateRun(ctx, "pc1_parent", "parent", nil); err != nil {
			t.Fatal(err)
		}
		creator := store.AsParentedRunCreator(s)
		if creator == nil {
			t.Fatal("missing child creation seam")
		}
		adapter := store.WithRuntimeSemantics(ctx, store.RuntimeSemanticsLegacyAdapterV1)
		if _, err := creator.CreateChildRun(adapter, "pc1_child", "adapter", "pc1_parent", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := creator.CreateChildRun(testCtx(), "escaped_child", "legacy", "pc1_parent", nil); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("child escaped: %v", err)
		}
		if err := s.SetSubbotChild(ctx, "pc1_parent", "node", "escaped_child"); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("child reference escaped: %v", err)
		}
		if err := s.SetSubbotChild(ctx, "pc1_parent", "node", "pc1_child"); err != nil {
			t.Fatal(err)
		}
		ids, err := s.ListChildRuns(ctx, "pc1_parent")
		if err != nil || !reflect.DeepEqual(ids, []string{"pc1_child"}) {
			t.Fatalf("children: %v, %v", ids, err)
		}
	})
	t.Run("MetadataClosure", func(t *testing.T) {
		s := factory(t)
		ctx := store.WithRuntimeSemantics(testCtx(), store.RuntimeSemanticsPortsV1)
		const id = "pc1_closure"
		if _, err := s.CreateRun(ctx, id, "test", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRun(testCtx(), "legacy_control", "legacy", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, NodeID: "producer"}); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteArtifact(ctx, &store.Artifact{RunID: id, NodeID: "producer", Version: 1, Data: map[string]any{"report": "ready"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteInteraction(ctx, &store.Interaction{ID: "question", RunID: id, NodeID: "producer", Questions: map[string]any{"confirm": "publish?"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendQueuedMessage(ctx, id, store.QueuedUserMessage{ID: "message", Text: "continue", Status: store.QueuedMessageStatusQueued, QueuedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		notes := store.AsRunNoteStore(s)
		if notes == nil {
			t.Fatal("missing notes seam")
		}
		if _, err := notes.AppendRunNote(ctx, id, store.RunNote{Body: "native note"}); err != nil {
			t.Fatal(err)
		}
		plans := store.AsPlanStore(s)
		if plans == nil {
			t.Fatal("missing plans seam")
		}
		if _, _, err := plans.AppendPlanSnapshot(ctx, id, store.PlanSnapshot{NodeID: "producer", Todos: []store.PlanTodo{{Content: "publish", Status: "pending"}}}); err != nil {
			t.Fatal(err)
		}
		turns := store.AsTurnStore(s)
		if turns == nil {
			t.Fatal("missing turns seam")
		}
		if err := turns.WriteTurn(ctx, &store.TurnCheckpoint{RunID: id, NodeID: "producer", Backend: "claw", TurnIndex: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveCheckpoint(ctx, id, &store.Checkpoint{NodeID: "producer"}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateRunStatus(ctx, id, store.RunStatusFailedResumable, "retry"); err != nil {
			t.Fatal(err)
		}
		tagsStore := store.AsRunTagStore(s)
		if tagsStore == nil {
			t.Fatal("missing tags seam")
		}
		if err := tagsStore.SetRunTags(ctx, id, []string{"native"}); err != nil {
			t.Fatal(err)
		}
		if ids, err := s.ListRuns(testCtx()); err != nil || !slices.Contains(ids, id) || !slices.Contains(ids, "legacy_control") || len(ids) != 2 {
			t.Fatalf("combined list: %v, %v", ids, err)
		}
		if events, err := s.LoadEvents(ctx, id); err != nil || len(events) < 1 {
			t.Fatalf("events: %v, %v", events, err)
		}
		if a, err := s.LoadArtifact(ctx, id, "producer", 1); err != nil || a.Data["report"] != "ready" {
			t.Fatalf("artifact: %+v, %v", a, err)
		}
		if i, err := s.LoadInteraction(ctx, id, "question"); err != nil || i.Questions["confirm"] != "publish?" {
			t.Fatalf("interaction: %+v, %v", i, err)
		}
		if rows, err := s.ListQueuedMessages(ctx, id); err != nil || len(rows) != 1 {
			t.Fatalf("messages: %+v, %v", rows, err)
		}
		if rows, err := notes.ListRunNotes(ctx, id); err != nil || len(rows) != 1 {
			t.Fatalf("notes: %+v, %v", rows, err)
		}
		if rows, err := plans.ListPlanSnapshots(ctx, id); err != nil || len(rows) != 1 {
			t.Fatalf("plans: %+v, %v", rows, err)
		}
		if rows, err := turns.ListTurns(ctx, id, "producer", 0); err != nil || len(rows) != 1 {
			t.Fatalf("turns: %+v, %v", rows, err)
		}
		if tags, err := tagsStore.GetRunTags(ctx, id); err != nil || !reflect.DeepEqual(tags, []string{"native"}) {
			t.Fatalf("tags: %v, %v", tags, err)
		}
		if err := s.DeleteRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadRun(ctx, id); !errors.Is(err, store.ErrRunDeleted) {
			t.Fatalf("deleted identity: %v", err)
		}
		if _, err := s.LoadRun(testCtx(), "legacy_control"); err != nil {
			t.Fatalf("native delete touched legacy: %v", err)
		}
		if err := s.DeleteRun(ctx, id); err != nil {
			t.Fatalf("idempotent delete: %v", err)
		}
	})
}
