package runner

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// answeringStore is the filesystem store with the probe-answer capability
// (store.RunProbeAnswerStore): it records every answer the runner writes. The
// write's own filter — the marker held, no answer yet — is the store's
// (pkg/store/mongo, mirrored by internal/retryprobetest and paired by their
// conformance test); what is under test here is whether the attempt writes.
type answeringStore struct {
	store.RunStore
	mu      sync.Mutex
	answers []string
}

func (s *answeringStore) AnswerRunProbe(_ context.Context, runID string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, runID)
	return true, nil
}

func (s *answeringStore) answered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.answers...)
}

// The attempt that follows a pull of its wait answers its credential window's
// probe — the sweeper releases the window's other waits on that record and on
// nothing else — exactly when a call it books on one of the wall's
// credentials was served. Driven through executeRun, which reads the record
// the attempt follows, then through the emitter it returns, with the events
// the backends emit:
//
//   - a claw step on the wall's credential, its keys on the event: answered;
//   - a claw step on another credential — the resume re-resolved onto a team
//     key or another pool slot: no answer, the wall was never asked;
//   - a claw step that spent nothing on the wall's credential: no answer;
//   - a claude_code delegation that finished, keyless, on the plan's row for
//     the wall's credential: answered;
//   - the same delegation that failed (delegate_error) after spending: no
//     answer — its spend may end on the very refusal the probe asks about;
//   - the same delegation on the pod's ambient env: no answer, whatever the
//     plan predicted;
//   - a step on the wall's credential in an attempt that follows no pull: no
//     watch, no answer.
func TestProbeAnswer_TheAttemptAfterAPullAnswersOnlyAServedCallOnTheWallsCredential(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	wallKey := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "zai", Fingerprint: "fp-wall-zai"}
	otherKey := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "zai", Fingerprint: "fp-team-zai"}
	step := func(spend credroute.Spend, in, out float64) store.Event {
		data := map[string]any{"input_tokens": in, "output_tokens": out, "provider": "zai"}
		credroute.PutEventKeys(data, spend)
		return store.Event{Type: store.EventLLMStepFinished, NodeID: "agent", Data: data}
	}
	delegation := func(typ store.EventType, label string) store.Event {
		return store.Event{Type: typ, NodeID: "agent", Data: map[string]any{
			"backend": delegate.BackendClaudeCode, "route_model": "claude-sonnet-4-5", "provider": "anthropic",
			"fingerprint": label, "tokens": 1200.0, "cost_usd": 0.12,
		}}
	}
	plan := map[string]string{"agent|anthropic": "fp-wall-oauth"}
	for _, tc := range []struct {
		name   string
		pulled bool
		evt    store.Event
		want   bool
	}{
		{"claw step on the wall's credential", true, step(wallKey, 900, 120), true},
		{"claw step on another credential", true, step(otherKey, 900, 120), false},
		{"claw step that spent nothing", true, step(wallKey, 0, 0), false},
		{"finished delegation on the plan's row", true, delegation(store.EventDelegateFinished, "anthropic-oauth"), true},
		{"failed delegation on the plan's row", true, delegation(store.EventDelegateError, "anthropic-oauth"), false},
		{"finished delegation on the ambient env", true, delegation(store.EventDelegateFinished, "anthropic-env"), false},
		{"attempt that follows no pull", false, step(wallKey, 900, 120), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			st := &answeringStore{RunStore: fs}
			const runID = "run-probe-answer"
			if _, err := fs.CreateRun(ctx, runID, "main", nil); err != nil {
				t.Fatal(err)
			}
			run, err := fs.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			run.TenantID, run.OwnerID = "team-1", "u1"
			// The record the attempt follows: a wait on the wall met on the
			// two sealed credentials, pulled a minute ago — or never.
			pulledAt := time.Now().UTC().Add(-time.Minute)
			announced := time.Now().UTC().Add(4 * 24 * time.Hour)
			run.RetryState = &store.RunRetryState{
				Reason: store.RetryReasonUsageWindow, Wall: &announced,
				WallCredentials: []string{"fp-wall-oauth", "fp-wall-zai"},
			}
			if tc.pulled {
				run.RetryState.ProbeFrom, run.RetryState.LastProbeAt = &announced, &pulledAt
			}
			if err := fs.SaveRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			body, err := ast.MarshalFile(parsertest.Parse("main.bot", "workflow main:\n  entry: done\n").File)
			if err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.New(iterlog.LevelInfo, io.Discard)}}
			msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body, CredPlan: plan}
			var usage *metricsEmitter
			if err := r.executeRun(ctx, msg, &usage, nil); err != nil {
				t.Fatalf("executeRun: %v", err)
			}
			if usage == nil {
				t.Fatal("executeRun returned no emitter — the premise of this witness")
			}
			usage.observe(tc.evt)
			usage.waitProbeAnswer()
			got := st.answered()
			if tc.want && (len(got) != 1 || got[0] != runID) {
				t.Fatalf("answers = %v, want one for %s — a call on the wall's credential was served after the pull, and the window's waits are released on that record alone", got, runID)
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("answers = %v, want none — the wall was never asked, and a release on this answer sends every wait of the window into it", got)
			}
		})
	}
}
