package dispatcher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSubbotReattachRequiresOriginalIntent(t *testing.T) {
	for _, name := range []string{"missing", "foreign", "cancelled", "bound-legacy", "bound-exact"} {
		t.Run(name, func(t *testing.T) {
			st, run, dir := reviewParentAdapter(t)
			format := store.LegacyValueFormat
			if name == "bound-exact" {
				format = store.ExactValueFormat
			}
			parent, e := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "parent", WorkflowName: "parent", Format: format})
			if e != nil {
				t.Fatal(e)
			}
			setup, e := store.BindRunValueFormat(t.Context(), parent)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = st.CreateRun(t.Context(), "existing-child", "child", nil); e != nil {
				t.Fatal(e)
			}
			if _, e = st.AppendEvent(t.Context(), "existing-child", store.Event{Type: store.EventNodeFinished, NodeID: "done", Data: map[string]any{"output": map[string]any{"ok": true}}}); e != nil {
				t.Fatal(e)
			}
			if e = st.UpdateRunStatus(t.Context(), "existing-child", store.RunStatusFinished, ""); e != nil {
				t.Fatal(e)
			}
			if e = st.SetSubbotChild(setup, "parent", "child", "existing-child"); e != nil {
				t.Fatal(e)
			}
			ctx := setup
			var want error
			switch name {
			case "missing":
				ctx = t.Context()
				want = store.ErrValueFormatCapability
			case "foreign":
				ctx, e = store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "foreign", Format: format})
				if e != nil {
					t.Fatal(e)
				}
				want = store.ErrValueFormatChanged
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			out, e := run(ctx, runtime.SubbotRequest{Source: "child.bot", ParentRunID: "parent", NodeID: "child", ReattachKey: "child", WorkDir: dir})
			if want != nil {
				if !errors.Is(e, want) {
					t.Fatalf("REATTACH_INTENT_BYPASS: err=%v want=%v out=%v", e, want, out)
				}
			} else if e != nil || out["ok"] != true {
				t.Fatalf("EXISTING_REATTACH_REFUSED: %v %v", e, out)
			}
			ids, e := st.ListRuns(t.Context())
			if e != nil || len(ids) != 2 {
				t.Fatalf("REATTACH_ROW_CHANGED: %v %v", ids, e)
			}
			got, e := st.LoadRun(setup, "parent")
			wantLink := ""
			if want != nil {
				wantLink = "existing-child"
			}
			if e != nil || got.SubbotChildren["child"] != wantLink {
				t.Fatalf("REATTACH_LINK_CHANGED: %v %v", got, e)
			}
		})
	}
}
