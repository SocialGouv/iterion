package runstream

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSPlanHistoricalInitialDirectoryAbsent(t *testing.T) {
	t.Run("events_wait_for_first_birth", func(t *testing.T) {
		src, st, root := newFileSourceFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		defer src.Close()
		sub, e := src.SubscribeEvents(ctx, "late", 0)
		if e != nil {
			t.Fatal(e)
		}
		defer sub.Close()
		if _, e = os.Stat(filepath.Join(root, "runs", "late")); !errors.Is(e, os.ErrNotExist) {
			t.Fatalf("directory existed at subscribe return: %v", e)
		}
		if _, e = st.CreateRun(ctx, "late", "wf", nil); e != nil {
			t.Fatal(e)
		}
		if _, e = st.AppendEvent(ctx, "late", store.Event{Data: map[string]any{"marker": "first-observation"}}); e != nil {
			t.Fatal(e)
		}
		select {
		case rows, ok := <-sub.Events():
			if !ok || len(rows) != 1 || rows[0].Data["marker"] != "first-observation" {
				t.Fatalf("bad delivery %#v %v", rows, ok)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no late event")
		}
		cancel()
		sub.Close()
		select {
		case _, ok := <-sub.Events():
			if ok {
				t.Fatal("unexpected extra event")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("producer not stopped")
		}
	})
	t.Run("logs_close_before_first_birth", func(t *testing.T) {
		src, _, root := newFileSourceFixture(t)
		defer src.Close()
		sub, e := src.SubscribeLogs(t.Context(), "late", 0)
		if e != nil {
			t.Fatal(e)
		}
		defer sub.Close()
		if _, e = os.Stat(filepath.Join(root, "runs", "late")); !errors.Is(e, os.ErrNotExist) {
			t.Fatal(e)
		}
		select {
		case _, ok := <-sub.Chunks():
			if ok {
				t.Fatal("unexpected log")
			}
		case <-time.After(time.Second):
			t.Fatal("missing log did not close")
		}
	})
}
