package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

func TestAssistantWatchReceiverAuthorizationPrecedesMutation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		caller      string
		owner       string
		mode        string
		kind        auth.IdentityKind
		incumbent   bool
		incapable   bool
		nonchat     bool
		foreign     bool
		midturn     bool
		disableAuth bool
		want        int
	}{
		{name: "foreign receiver create", caller: "alice", owner: "bob", mode: "cloud", want: http.StatusForbidden},
		{name: "foreign receiver transfer", caller: "alice", owner: "bob", mode: "cloud", incumbent: true, want: http.StatusForbidden},
		{name: "missing cloud identity", owner: "alice", mode: "cloud", want: http.StatusForbidden},
		{name: "webhook identity", caller: "alice", owner: "alice", mode: "cloud", kind: auth.KindWebhook, want: http.StatusForbidden},
		{name: "share identity", caller: "alice", owner: "alice", mode: "cloud", kind: auth.KindShare, want: http.StatusForbidden},
		{name: "foreign tenant", caller: "alice", owner: "alice", mode: "cloud", foreign: true, want: http.StatusForbidden},
		{name: "incapable reconfiguration", caller: "alice", owner: "alice", mode: "cloud", incumbent: true, incapable: true, want: http.StatusConflict},
		{name: "nonchat receiver", caller: "alice", owner: "alice", mode: "cloud", nonchat: true, want: http.StatusConflict},
		{name: "own paused chat", caller: "alice", owner: "alice", mode: "cloud", want: http.StatusOK},
		{name: "own midturn chat", caller: "alice", owner: "alice", mode: "cloud", midturn: true, want: http.StatusOK},
		{name: "local owner fallback", owner: "local-owner", want: http.StatusOK},
		{name: "local legacy empty owner", want: http.StatusOK},
		{name: "local dev legacy empty owner", caller: "dev", disableAuth: true, want: http.StatusOK},
		{name: "authenticated local foreign owner", caller: "alice", owner: "bob", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArmFixture(t, !tc.incapable)
			a := f.assistant(t, "assistant", "card")
			target := f.target(t, "target", "card", a.CreatedAt.Add(time.Minute))
			a.OwnerID, a.TenantID, target.TenantID = tc.owner, "tenant", "tenant"
			if tc.mode != "cloud" {
				a.TenantID, target.TenantID = "", ""
			}
			if tc.foreign {
				a.TenantID = "foreign"
			}
			if tc.nonchat {
				a.BotID, a.FilePath = "missing-nonchat-bot", ""
			}
			if tc.midturn {
				a.Status, a.Checkpoint = store.RunStatusRunning, nil
			}
			target.Status = store.RunStatusRunning
			for _, r := range []*store.Run{a, target} {
				if err := f.rs.SaveRun(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			}
			f.srv.cfg.Mode, f.srv.cfg.DisableAuth = tc.mode, tc.disableAuth
			f.srv.assistantWatch = f.coord
			f.coord.worker = "" // This test exercises the synchronous admission boundary.
			var resumes atomic.Int64
			f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
				resumes.Add(1)
				return &runview.LaunchResult{}, nil
			}
			if tc.incumbent {
				receiver := a.ID
				if tc.owner != tc.caller {
					receiver = "old-assistant"
				}
				if err := f.ws.CreateWatch(t.Context(), runwatch.Watch{ID: "incumbent", TenantID: target.TenantID, OwnerID: tc.caller, TargetRunID: target.ID, AssistantRunID: receiver, State: runwatch.WatchActive}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.ws.ListActive(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(t.Context(), target.TenantID, tc.caller)
			if tc.caller != "" || tc.kind != "" {
				ctx = auth.WithIdentity(ctx, auth.Identity{UserID: tc.caller, TeamID: target.TenantID, Kind: tc.kind})
			}
			beforeSnapshot, err := f.srv.runs.SnapshotCtx(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/runs/target/assistant-watches", strings.NewReader(`{"assistant_run_id":"assistant","mode":"propose"}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", target.ID)
			rec := httptest.NewRecorder()
			f.srv.handleCreateAssistantWatch(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("response=%d %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if tc.want == http.StatusOK {
				return
			}
			after, err := f.ws.ListActive(t.Context(), 0)
			if err != nil || !reflect.DeepEqual(before, after) || resumes.Load() != 0 {
				t.Fatalf("denied request changed watches or resumed: before=%+v after=%+v resumes=%d err=%v", before, after, resumes.Load(), err)
			}
			afterSnapshot, err := f.srv.runs.SnapshotCtx(ctx, a.ID)
			if err != nil || afterSnapshot.LastSeq != beforeSnapshot.LastSeq {
				t.Fatalf("denied request emitted assistant events: %v", err)
			}
		})
	}
}

func TestAssistantWatchSweepReconcilesBeyondFirstPage(t *testing.T) {
	f := newArmFixture(t, true)
	a := f.assistant(t, "assistant", "card")
	target := f.target(t, "last-target", "card", a.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	for i := 0; i <= 500; i++ {
		targetID := fmt.Sprintf("missing-%03d", i)
		if i == 500 {
			targetID = target.ID
		}
		if err := f.ws.CreateWatch(t.Context(), runwatch.Watch{
			ID: fmt.Sprintf("w%03d", i), TargetRunID: targetID, AssistantRunID: a.ID,
			State: runwatch.WatchActive, Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var resumes int
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		if spec.RunID != a.ID {
			t.Fatalf("foreign resume: %s", spec.RunID)
		}
		resumes++
		return &runview.LaunchResult{}, nil
	}
	f.coord.sweep(t.Context())
	w, err := f.ws.GetWatch(t.Context(), "w500")
	if err != nil || w.DeliveredEpisodes != 1 || resumes != 1 {
		t.Fatalf("watch beyond first page not recovered: %+v resumes=%d err=%v", w, resumes, err)
	}
	first, err := f.ws.GetWatch(t.Context(), "w000")
	if err != nil || first.State != runwatch.WatchStopped {
		t.Fatalf("first page cleanup failed: %+v err=%v", first, err)
	}
}

// Cancel as a second page arrives: the pass must neither process its rows
// nor publish a successful heartbeat after an incomplete traversal.
type cancellingWatchPage struct {
	runwatch.Store
	cancel context.CancelFunc
	calls  int
}

func (s *cancellingWatchPage) ListActivePage(ctx context.Context, after *runwatch.WatchCursor, through runwatch.WatchCursor, limit int) ([]runwatch.Watch, error) {
	s.calls++
	if after != nil {
		s.cancel()
		return []runwatch.Watch{{ID: "never-process"}}, nil
	}
	rows := make([]runwatch.Watch, 500)
	for i := range rows {
		rows[i] = runwatch.Watch{ID: fmt.Sprint(i), TenantID: "", CreatedAt: through.CreatedAt}
	}
	return rows, nil
}
func TestAssistantWatchCancelledPageDoesNotCompleteSweep(t *testing.T) {
	f := newArmFixture(t, true)
	if err := f.ws.CreateWatch(t.Context(), runwatch.Watch{ID: "upper", State: runwatch.WatchActive}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	paging := &cancellingWatchPage{Store: f.ws, cancel: cancel}
	f.coord.setRuntime(f.srv.runs, paging, f.srv.cfg.Bots.Paths)
	// These rows have no cloud tenant, so processing the first page does not
	// need 500 run fixtures. The second page cancels the captured context.
	f.srv.cfg.Mode = "cloud"
	f.coord.sweep(ctx)
	if paging.calls != 2 || !f.coord.lastSweepCompletedAt.IsZero() || f.coord.lastSweepStartedAt.IsZero() {
		t.Fatalf("cancelled pass published completion: calls=%d started=%v completed=%v", paging.calls, f.coord.lastSweepStartedAt, f.coord.lastSweepCompletedAt)
	}
}

type failingWatchCompletion struct {
	runwatch.Store
	calls int
}

func (s *failingWatchCompletion) CompleteEpisode(context.Context, string, string, time.Time) error {
	s.calls++
	return errors.New("injected accounting failure")
}
func TestAssistantWatchCompletionFailureKeepsFinishedTargetRetryable(t *testing.T) {
	f := newArmFixture(t, true)
	a := f.assistant(t, "assistant", "card")
	target := f.target(t, "target", "card", a.CreatedAt.Add(time.Minute))
	target.Status = store.RunStatusFinished
	if err := f.rs.SaveRun(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := runwatch.Watch{ID: "watch", TargetRunID: target.ID, AssistantRunID: a.ID, State: runwatch.WatchActive, CreatedAt: now}
	if err := f.ws.CreateWatch(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	ep := runwatch.Episode{ID: "episode", WatchID: w.ID, TargetRunID: target.ID, AssistantRunID: a.ID, Kind: trigger.KindRunFinished, OutcomeEventID: "finished", State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now}
	if _, err := f.ws.CreateEpisode(t.Context(), ep); err != nil {
		t.Fatal(err)
	}
	failing := &failingWatchCompletion{Store: f.ws}
	f.coord.setRuntime(f.srv.runs, failing, f.srv.cfg.Bots.Paths)
	resumes := 0
	f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
		resumes++
		return &runview.LaunchResult{}, nil
	}
	f.coord.attempt(t.Context(), ep.ID)
	got, err := f.ws.GetWatch(t.Context(), w.ID)
	if err != nil || resumes != 1 || failing.calls != 1 || got.State != runwatch.WatchActive || got.DeliveredEpisodes != 0 {
		t.Fatalf("failed accounting stranded watch: %+v resumes=%d completions=%d err=%v", got, resumes, failing.calls, err)
	}
	if _, won, err := f.ws.ClaimEpisode(t.Context(), ep.ID, "retry", now.Add(2*assistantWatchLease), assistantWatchLease); err != nil || !won {
		t.Fatalf("retry claim=%v err=%v", won, err)
	}
}
