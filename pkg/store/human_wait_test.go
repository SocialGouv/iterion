package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

type unreadableHumanWaitStore struct {
	RunStore
	unreadable string
}

func (s unreadableHumanWaitStore) LoadRun(ctx context.Context, id string) (*Run, error) {
	if id == s.unreadable {
		return nil, errors.New("unreadable run")
	}
	return s.RunStore.LoadRun(ctx, id)
}

func TestHasBlockingHumanWait(t *testing.T) {
	for _, mode := range []string{"pending-question", "root-wait", "expired-wait", "paused-child", "paused-grandchild", "terminal-child", "terminal-root", "cycle", "unreadable", "invalid-marker"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			now := time.Now()
			for _, id := range []string{"root", "child", "grandchild"} {
				if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
					t.Fatal(err)
				}
			}
			link := func(parent, key, child string) {
				t.Helper()
				if err := s.SetSubbotChild(ctx, parent, key, child); err != nil {
					t.Fatal(err)
				}
			}
			state := func(id string, status RunStatus) {
				t.Helper()
				if err := s.UpdateRunStatus(ctx, id, status, ""); err != nil {
					t.Fatal(err)
				}
			}
			var reader RunStore = s
			want := false
			switch mode {
			case "pending-question":
				if err := s.WriteInteraction(ctx, &Interaction{ID: "q", RunID: "root", NodeID: "agent", Kind: InteractionKindAsync, RequestedAt: now}); err != nil {
					t.Fatal(err)
				}
			case "root-wait", "expired-wait":
				until := now.Add(time.Minute)
				want = true
				if mode == "expired-wait" {
					until = now
					want = false
				}
				if err := s.SetAwaitAnswersWait(ctx, "root", "execution", &AwaitAnswersWait{NodeID: "sync", Until: until}); err != nil {
					t.Fatal(err)
				}
			case "paused-child", "terminal-root", "unreadable":
				link("root", "subbot", "child")
				state("child", RunStatusPausedWaitingHuman)
				want = mode == "paused-child"
				if mode == "terminal-root" {
					state("root", RunStatusFinished)
				}
				if mode == "unreadable" {
					reader = unreadableHumanWaitStore{s, "child"}
				}
			case "paused-grandchild", "terminal-child":
				link("root", "subbot", "child")
				link("child", "subbot", "grandchild")
				state("grandchild", RunStatusPausedWaitingHuman)
				want = mode == "paused-grandchild"
				if mode == "terminal-child" {
					state("child", RunStatusFinished)
				}
			case "cycle":
				link("root", "subbot", "child")
				link("child", "back", "root")
			case "invalid-marker":
				r, err := s.LoadRun(ctx, "root")
				if err != nil {
					t.Fatal(err)
				}
				r.AwaitAnswersWaits = map[string]AwaitAnswersWait{"broken": {Until: now.Add(time.Hour)}}
				if err := s.SaveRun(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if got := HasBlockingHumanWait(ctx, reader, "root", now); got != want {
				t.Fatalf("waiting=%v, want %v", got, want)
			}
		})
	}
}
