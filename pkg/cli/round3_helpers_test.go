package cli_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

func r3WriteBot(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "probe.bot")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type r3RunResult struct {
	err    error
	run    *store.Run
	events []*store.Event
	store  store.RunStore
	dir    string
}

func r3RunBot(t *testing.T, path, runID string) r3RunResult {
	t.Helper()
	storeDir := t.TempDir()
	p := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}
	err := cli.RunRun(context.Background(), cli.RunOptions{File: path, StoreDir: storeDir, RunID: runID, NoInteractive: true, MergeInto: "none"}, p)
	st, serr := store.New(storeDir)
	if serr != nil {
		t.Fatal(serr)
	}
	run, lerr := st.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("load run: %v (run err %v)", lerr, err)
	}
	evs, _ := st.LoadEvents(context.Background(), runID)
	return r3RunResult{err: err, run: run, events: evs, store: st, dir: storeDir}
}

func r3FmtAny(v any) string { return fmt.Sprintf("%v", v) }
