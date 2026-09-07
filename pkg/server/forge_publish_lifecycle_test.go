package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// A saturated registry used to be SILENT: Register errored, the launch went
// ahead with no grant, and the reconciler then abstained ("not a gating run")
// on a head the launch was about to claim — the "pending forever" shape. The
// launch is refused instead, before it claims anything.
func TestInjectForgePublishVarsRefusesTheLaunchWhenTheGrantCannotBeMinted(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.example"
	s.forgePublishTokens = &failingPublishTokenStore{
		ForgePublishTokenStore: NewForgePublishTokenRegistry(),
		err:                    fmt.Errorf("forge publish token registry full (%d tokens)", forgePublishMaxTokens),
	}

	vars := map[string]string{"pr_url": "https://github.com/o/r/pull/42"}
	out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", vars, nil)
	if err == nil {
		t.Fatalf("a grant that cannot be minted must refuse the launch, got vars=%v", out)
	}
	if !errors.Is(err, errForgePublishGrantUnavailable) {
		t.Errorf("err = %v, want errForgePublishGrantUnavailable so the lanes can type it", err)
	}
	if _, ok := out[forgePublishVarToken]; ok {
		t.Error("no token must be handed out when the registry refused it")
	}
}

// failingPublishTokenStore fails every Register while delegating the rest.
type failingPublishTokenStore struct {
	ForgePublishTokenStore
	err error
}

func (f *failingPublishTokenStore) Register(string, ForgePublishGrant) error { return f.err }

// The grant's only bound was its TTL — ~10 days, because it has to outlive the
// longest usage-window retry. A run that ENDED needs it only until the gate
// reconciler's sweep has had its window, so the terminal outcome shortens it.
func TestForgePublishGrantExpiresOnTheRunsTerminalOutcome(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*store.Run)
		wantShort bool
	}{
		{"finished", func(r *store.Run) { r.Status = store.RunStatusFinished }, true},
		{"failed", func(r *store.Run) { r.Status = store.RunStatusFailed }, true},
		{"cancelled", func(r *store.Run) { r.Status = store.RunStatusCancelled }, true},
		{
			// Nothing is coming back for it: budget exceeded, retries
			// exhausted, a plain execution failure. The retry sweeper's
			// abandon leaves exactly this shape (retry_after unset).
			"failed_resumable with no armed retry",
			func(r *store.Run) { r.Status = store.RunStatusFailedResumable },
			true,
		},
		{
			// The retry sweeper will resume it and it will post its own
			// verdict — it still needs the grant.
			"failed_resumable with an armed retry",
			func(r *store.Run) {
				at := time.Now().UTC().Add(time.Hour)
				r.Status = store.RunStatusFailedResumable
				r.RetryState = &store.RunRetryState{RetryAfter: &at, Reason: "usage_window"}
			},
			false,
		},
		{
			// A paused run is expected to resume.
			"paused",
			func(r *store.Run) { r.Status = store.RunStatusPausedWaitingHuman },
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s, _ := newForgePublishTestServer(t)
			s.cfg.Store = st
			const token = "tok-life"
			registerPublishToken(t, s, token, ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			if _, err := st.CreateRun(context.Background(), "run-life", "review_pr", map[string]any{
				forgePublishVarToken: token, "pr_url": "https://github.com/o/r/pull/42",
			}); err != nil {
				t.Fatal(err)
			}
			run, err := st.LoadRun(context.Background(), "run-life")
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(run)
			if err := st.SaveRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}

			if err := s.expireForgePublishGrantForRun(context.Background(), "run-life"); err != nil {
				t.Fatal(err)
			}
			g, ok := s.forgePublishTokens.lookup(token)
			if !ok {
				t.Fatal("the grant must stay usable through the repair window, not vanish")
			}
			left := time.Until(g.ExpiresAt)
			switch {
			case tc.wantShort && left > forgePublishPostRunGrace+time.Minute:
				t.Errorf("grant still lives %s after a terminal outcome, want ≤ %s", left, forgePublishPostRunGrace)
			case !tc.wantShort && left <= forgePublishPostRunGrace+time.Minute:
				t.Errorf("grant shortened to %s on a run that will resume and publish its own verdict", left)
			}
		})
	}
}

// The eventbus is the trigger: the same run-outcome event the notification
// dispatcher and the gate reconciler consume.
func TestForgePublishGrantExpiryRidesTheRunOutcomeEvent(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newForgePublishTestServer(t)
	s.cfg.Store = st
	bus := eventbus.NewInProcBus(iterlog.New(iterlog.LevelError, nil))

	const token = "tok-bus"
	registerPublishToken(t, s, token, ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	if _, err := st.CreateRun(context.Background(), "run-bus", "review_pr", map[string]any{
		forgePublishVarToken: token, "pr_url": "https://github.com/o/r/pull/42",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(context.Background(), "run-bus")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFinished
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	cancel, err := s.attachForgePublishGrantExpiry(bus)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	if err := bus.Publish(context.Background(), trigger.Event{
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunFinished,
		Subject: trigger.Subject{ID: "run-bus"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		g, ok := s.forgePublishTokens.lookup(token)
		if ok && time.Until(g.ExpiresAt) <= forgePublishPostRunGrace+time.Minute {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run-outcome event did not shorten the grant (ok=%v)", ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
