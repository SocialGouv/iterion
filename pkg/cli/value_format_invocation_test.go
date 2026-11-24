package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCLIPreparationRefusesBeforeAcquiringExecutor(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name          string
		actual, bound store.ValueFormat
		background    bool
		want          error
	}{
		{"missing pickup", 0, 1, true, store.ErrRunNotFound},
		{"legacy pickup exact successor", 2, 1, true, store.ErrValueFormatChanged},
		{"exact pickup legacy successor", 1, 2, true, store.ErrValueFormatChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "launch.bot")
			writeFile(t, path, contextLaunchSource)
			st, err := store.New(filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.actual != 0 {
				if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: tc.actual, Inputs: map[string]any{"original": "kept"}}); err != nil {
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
			ex := &contextLaunchCloser{}
			err = RunRun(ctx, RunOptions{File: path, RunID: "run", Background: tc.background, StoreDir: st.Root(), Executor: ex, NoInteractive: true, Sandbox: "none"}, &Printer{W: io.Discard, Format: OutputJSON})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want %v", err, tc.want)
			}
			if ex.closed {
				t.Fatal("executor acquired on refusal")
			}
			after, _ := os.ReadFile(doc)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed durable run")
			}
			for _, file := range []string{"run.log", "run.pid"} {
				if _, err = os.Stat(filepath.Join(st.Root(), "runs", "run", file)); !os.IsNotExist(err) {
					t.Fatalf("refusal touched %s: %v", file, err)
				}
			}
		})
	}
}

func TestCLIExplicitFreshIDIsBornBeforeInjectedExecutor(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.bot")
	writeFile(t, path, contextLaunchSource)
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	ex := &contextLaunchCloser{}
	if err = RunRun(t.Context(), RunOptions{File: path, RunID: "fresh", StoreDir: st.Root(), Executor: ex, NoInteractive: true, Sandbox: "none"}, &Printer{W: io.Discard, Format: OutputJSON}); err != nil {
		t.Fatal(err)
	}
	if !ex.closed {
		t.Fatal("positive executor not acquired")
	}
	r, err := st.LoadRun(t.Context(), "fresh")
	if err != nil || r.FormatVersion != 1 || r.Status != store.RunStatusFinished {
		t.Fatalf("run=%+v error=%v", r, err)
	}
}

func TestCLIExplicitExistingIDKeepsItsRun(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.bot")
	writeFile(t, path, contextLaunchSource)
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.CreateRun(t.Context(), "existing", "fixture", map[string]any{"original": "kept"})
	if err != nil {
		t.Fatal(err)
	}
	ex := &contextLaunchCloser{}
	if err = RunRun(t.Context(), RunOptions{File: path, RunID: old.ID, StoreDir: st.Root(), Executor: ex, NoInteractive: true, Sandbox: "none"}, &Printer{W: io.Discard, Format: OutputJSON}); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadRun(t.Context(), old.ID)
	if err != nil || !ex.closed || got.Inputs["original"] != "kept" || got.Status != store.RunStatusFinished || !got.CreatedAt.Equal(old.CreatedAt) {
		t.Fatalf("existing pickup %+v %v closed=%v", got, err, ex.closed)
	}
}
