package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const answersKey = "hk-live-9f8e7d6c5b4a3210-FAKE"

// scrubbingExecutor is a stub executor with the SecretScrubber of a run
// that knows one secret.
type scrubbingExecutor struct {
	*stubExecutor
	guard *secretguard.Guard
}

func (s scrubbingExecutor) ScrubOutput(m map[string]any) map[string]any { return s.guard.RedactMap(m) }

func newScrubbingEngine(t *testing.T, runID string) (*Engine, store.RunStore) {
	t.Helper()
	s := tmpStore(t)
	if _, err := s.CreateRun(context.Background(), runID, "review_test", nil); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: answersKey}}, secretguard.DefaultConfig())
	return New(minimalReviewWorkflow(), s, scrubbingExecutor{newStubExecutor(), guard}), s
}

// eventsOfType returns the JSON of every event of typ in the run's log.
func eventsOfType(t *testing.T, s store.RunStore, runID string, typ store.EventType) []string {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Type == typ {
			b, _ := json.Marshal(ev.Data)
			out = append(out, string(b))
		}
	}
	if len(out) == 0 {
		t.Fatalf("scenario broken: no %s event", typ)
	}
	return out
}

// A review companion's verdict quoting a value it read: the review_verdict
// event is scrubbed like a node's output.
func TestAReviewVerdictIsScrubbedInTheEventLog(t *testing.T) {
	e, s := newScrubbingEngine(t, "verdict-run")
	e.emitReviewTurn(context.Background(), "verdict-run", "gate", "companion", 2, map[string]any{
		"decision": "changes_requested",
		"blockers": []any{"config.yml:3 commits the hook key " + answersKey},
	})
	for _, ev := range eventsOfType(t, s, "verdict-run", store.EventReviewVerdict) {
		if strings.Contains(ev, answersKey) {
			t.Errorf("review_verdict carries the value: %s", ev)
		}
	}
}

// A review gate's verdict — the companion's blockers merged in, or the
// agent's own verdict — is recorded on the interaction whole and in the event
// log scrubbed.
func TestAReviewGatesRecordedVerdictIsScrubbedInTheEventLog(t *testing.T) {
	ctx := context.Background()
	e, s := newScrubbingEngine(t, "gate-run")
	if err := s.WriteInteraction(ctx, &store.Interaction{ID: "gate-ix", RunID: "gate-run", NodeID: "gate"}); err != nil {
		t.Fatal(err)
	}
	rs := e.newRunState("gate-run", nil)
	rs.ctx = ctx
	hn, _ := e.workflow.Nodes["gate"].(*ir.HumanNode)
	verdict := map[string]any{"decision": "changes_requested", "blockers": []any{"rotate " + answersKey}}
	if next, err := e.gateSelectEdge(ctx, rs, hn, "gate", "gate-ix", verdict); err != nil || next != "done" {
		t.Fatalf("scenario broken: next=%q err=%v", next, err)
	}
	for _, ev := range eventsOfType(t, s, "gate-run", store.EventHumanAnswersRecorded) {
		if strings.Contains(ev, answersKey) {
			t.Errorf("human_answers_recorded carries the value: %s", ev)
		}
	}
	it, err := s.LoadInteraction(ctx, "gate-run", "gate-ix")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(it.Answers); !strings.Contains(string(b), answersKey) {
		t.Errorf("the interaction lost the verdict as given: %s", b)
	}
}

// The answers a resume records are kept whole on the interaction — the run
// resumes with them — and scrubbed in the event log.
func TestARecordedAnswerIsScrubbedInTheEventLog(t *testing.T) {
	ctx := context.Background()
	e, s := newScrubbingEngine(t, "answers-run")
	r, err := s.LoadRun(ctx, "answers-run")
	if err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "gate", InteractionID: "answers-ix", InteractionQuestions: map[string]any{"note": "What changed?"}}
	answers, err := e.recordHumanAnswers(ctx, r, cp, map[string]any{"note": "rotated " + answersKey})
	if err != nil {
		t.Fatal(err)
	}
	if note, _ := answers["note"].(string); !strings.Contains(note, answersKey) {
		t.Errorf("the run resumes with altered answers: %v", answers)
	}
	for _, ev := range eventsOfType(t, s, "answers-run", store.EventHumanAnswersRecorded) {
		if strings.Contains(ev, answersKey) {
			t.Errorf("human_answers_recorded carries the value: %s", ev)
		}
	}
}

// The recorded answers' event names its interaction as is: the scrub runs over
// the answers only, so even an id that happens to hold a registered value
// still keys the event (the studio flips its card on it).
func TestARecordedAnswersEventKeepsItsInteractionID(t *testing.T) {
	ctx := context.Background()
	const id = "run-7-gate-3"
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "id-run", "review_test", nil); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "tag", Value: id}}, secretguard.DefaultConfig())
	e := New(minimalReviewWorkflow(), s, scrubbingExecutor{newStubExecutor(), guard})
	r, err := s.LoadRun(ctx, "id-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.recordHumanAnswers(ctx, r, &store.Checkpoint{NodeID: "gate", InteractionID: id, InteractionQuestions: map[string]any{"q": "?"}}, map[string]any{"note": "ok"}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range eventsOfType(t, s, "id-run", store.EventHumanAnswersRecorded) {
		if !strings.Contains(ev, `"interaction_id":"`+id+`"`) {
			t.Errorf("the event lost its interaction id: %s", ev)
		}
	}
}
