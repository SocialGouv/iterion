package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

const reviewParentChildSource = `schema checked:
  ok: bool
compute check:
  output: checked
  expr:
    ok: "true"
workflow probe:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`

func reviewParentAdapter(t *testing.T) (store.RunStore, runtime.SubbotRunner, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "child.bot"), []byte(reviewParentChildSource), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	run := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), st.Root(), st, iterlog.Nop(), RunOptions{Sandbox: "none"})
	return st, run, dir
}
func TestSubbotOriginalParentBinding(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		present, bound, cancel bool
		format                 store.ValueFormat
		boundID                string
		want                   error
	}{
		{"present-unbound-legacy", true, false, false, 1, "", store.ErrValueFormatCapability},
		{"present-bound-legacy", true, true, false, 1, "parent", nil},
		{"absent-bound-legacy", false, true, false, 1, "parent", store.ErrChildParentUnreadable},
		{"absent-unbound-legacy", false, false, false, 1, "", store.ErrValueFormatCapability},
		{"present-bound-exact", true, true, false, 2, "parent", store.ErrValueFormatCapability},
		{"foreign-binding", true, true, false, 1, "foreign", store.ErrValueFormatChanged},
		{"cancelled-bound-legacy", true, true, true, 1, "parent", context.Canceled},
		{"empty-parent-unbound", false, false, false, 1, "", store.ErrValueFormatCapability},
		{"empty-parent-bound", true, true, false, 1, "parent", store.ErrValueFormatChanged},
		{"changed-parent-current-exact", true, true, false, 2, "parent", store.ErrValueFormatChanged},
		{"changed-parent-original-exact", true, true, false, 1, "parent", store.ErrValueFormatChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, run, dir := reviewParentAdapter(t)
			const parentID = "parent"
			var original *store.Run
			if tc.present {
				if _, e := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: parentID, WorkflowName: "parent", Format: tc.format}); e != nil {
					t.Fatal(e)
				}
				var e error
				original, e = st.LoadRun(t.Context(), parentID)
				if e != nil {
					t.Fatal(e)
				}
			}
			ctx := t.Context()
			if tc.bound {
				var e error
				boundFormat := tc.format
				if tc.name == "changed-parent-current-exact" {
					boundFormat = 1
				}
				if tc.name == "changed-parent-original-exact" {
					boundFormat = 2
				}
				ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: tc.boundID, Format: boundFormat})
				if e != nil {
					t.Fatal(e)
				}
			}
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			requestParentID := parentID
			if tc.name == "empty-parent-unbound" || tc.name == "empty-parent-bound" {
				requestParentID = ""
			}
			out, e := run(ctx, runtime.SubbotRequest{Source: "child.bot", ParentRunID: requestParentID, NodeID: "child", ReattachKey: "child", WorkDir: dir})
			ids, le := st.ListRuns(t.Context())
			if le != nil {
				t.Fatal(le)
			}
			if tc.want != nil {
				if !errors.Is(e, tc.want) {
					t.Errorf("ORIGINAL_PARENT_ADOPTED error=%v want=%v out=%v rows=%v", e, tc.want, out, ids)
				}
				wantRows := 0
				if tc.present {
					wantRows = 1
				}
				if len(ids) != wantRows {
					t.Errorf("REFUSAL_CREATED_CHILD rows=%v", ids)
				}
				if tc.present {
					got, le := st.LoadRun(t.Context(), parentID)
					if le != nil {
						t.Fatal(le)
					}
					if !reflect.DeepEqual(got, original) {
						t.Errorf("REFUSAL_CHANGED_PARENT original=%v now=%v", original.SubbotChildren, got.SubbotChildren)
					}
				}
			} else {
				wantRows := 1
				if tc.present {
					wantRows = 2
				}
				if e != nil || len(ids) != wantRows || out["ok"] != true {
					t.Fatalf("BOUND_LEGACY_REFUSED err=%v out=%v rows=%v", e, out, ids)
				}
			}
		})
	}
}
func TestSubbotReattachesExactExistingChild(t *testing.T) {
	st, run, dir := reviewParentAdapter(t)
	parent, e := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "parent", WorkflowName: "parent", Format: 2})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := store.BindRunValueFormat(t.Context(), parent)
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
	if e = st.SetSubbotChild(ctx, "parent", "child", "existing-child"); e != nil {
		t.Fatal(e)
	}
	out, e := run(ctx, runtime.SubbotRequest{Source: "child.bot", ParentRunID: "parent", NodeID: "child", ReattachKey: "child", WorkDir: dir})
	ids, le := st.ListRuns(t.Context())
	if le != nil {
		t.Fatal(le)
	}
	if e != nil || out["ok"] != true || len(ids) != 2 {
		t.Fatalf("EXACT_REATTACH_FALSE_REFUSAL err=%v out=%v rows=%v", e, out, ids)
	}
}
