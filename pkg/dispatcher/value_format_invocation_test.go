package dispatcher

import (
	"bytes"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestDispatchPreparationPrecedesHooksAndLogs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		actual, bound store.ValueFormat
		resume        string
		want          error
	}{
		{"duplicate", 1, 0, "", fs.ErrExist}, {"pickup missing", 0, 1, "run", store.ErrRunNotFound}, {"pickup original legacy", 2, 1, "run", store.ErrValueFormatChanged}, {"pickup original exact", 1, 2, "run", store.ErrValueFormatChanged}, {"mismatched resume ID", 1, 1, "other", store.ErrValueFormatChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bot := filepath.Join(dir, "wf.bot")
			if err := os.WriteFile(bot, []byte("workflow probe:\n  worktree: none\n  sandbox: none\n  entry: done\n"), 0600); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.actual != 0 {
				if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: tc.actual}); err != nil {
					t.Fatal(err)
				}
			}
			doc := filepath.Join(st.Root(), "runs", "run", "run.json")
			before, _ := os.ReadFile(doc)
			ctx := t.Context()
			if tc.bound != 0 {
				ctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "run", Format: tc.bound})
				if err != nil {
					t.Fatal(err)
				}
			}
			r, err := NewEngineRunner(bot, iterlog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			events := 0
			err = r.Dispatch(ctx, DispatchSpec{RunID: "run", ResumeFromRunID: tc.resume, WorkspacePath: dir, StoreDir: st.Root(), OnEvent: func(string) { events++ }})
			if !errors.Is(err, tc.want) || events != 0 {
				t.Fatalf("error=%v want %v events=%d", err, tc.want, events)
			}
			after, _ := os.ReadFile(doc)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed successor")
			}
			if _, err = os.Stat(filepath.Join(st.Root(), "runs", "run", "run.log")); !os.IsNotExist(err) {
				t.Fatalf("refusal opened log: %v", err)
			}
		})
	}
}
