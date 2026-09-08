package delegate

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func sessionFailureBackend() *ClaudeCodeBackend {
	return &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
}

// A session that dies mid-stream never produces a ResultMessage, so the
// failure it returns could not name the session the CLI had already
// announced on `system/init` — the id was logged and dropped. Nothing above
// the delegate could then resume that session: the node started over from
// zero, and on a long agent node that is the whole run's budget.
func TestStreamFailureNamesTheSessionTheCLIAnnounced(t *testing.T) {
	task := Task{NodeID: "n", Iteration: 1, Model: "claude-opus-5"}
	b := sessionFailureBackend()

	t.Run("no result message: the streamed id is what the failure carries", func(t *testing.T) {
		res, err := b.buildStreamErrorResult(nil, sessionMeta{sessionID: "sess-from-init"},
			errors.New("session ended without result"), "fetch failed", time.Second, task)
		if err == nil {
			t.Fatal("a broken stream must still return an error")
		}
		if res.SessionID != "sess-from-init" {
			t.Fatalf("SessionID = %q, want the id system/init announced — a failure that cannot name its session forces the node to start over", res.SessionID)
		}
	})

	t.Run("a result message wins: the authoritative end of the session", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{SessionID: "sess-from-result"}
		res, _ := b.buildStreamErrorResult(rm, sessionMeta{sessionID: "sess-from-init"},
			errors.New("boom"), "", time.Second, task)
		if res.SessionID != "sess-from-result" {
			t.Fatalf("SessionID = %q, want the result message's", res.SessionID)
		}
	})

	t.Run("neither: nothing invented", func(t *testing.T) {
		res, _ := b.buildStreamErrorResult(nil, sessionMeta{}, errors.New("spawn failed"), "", time.Second, task)
		if res.SessionID != "" {
			t.Fatalf("SessionID = %q, want empty — a spawn that never opened a session has none", res.SessionID)
		}
	})
}

// The id is announced on `system/init`, but a stream that broke before it
// still names the session on whatever it did emit. First non-empty wins:
// the value never changes within a session, and re-reading later risks
// taking a sub-agent's.
func TestSystemMessageCapturesTheSessionOnAnySubtype(t *testing.T) {
	b := sessionFailureBackend()
	task := Task{NodeID: "n", Iteration: 1}

	var meta sessionMeta
	b.handleSystemMessage(&claudesdk.SystemMessage{Subtype: "hook", SessionID: "s-early"}, task, &meta)
	if meta.sessionID != "s-early" {
		t.Fatalf("sessionID = %q, want s-early — a non-init subtype names the session too", meta.sessionID)
	}
	b.handleSystemMessage(&claudesdk.SystemMessage{Subtype: "init", SessionID: "s-later", Model: "m"}, task, &meta)
	if meta.sessionID != "s-early" {
		t.Fatalf("sessionID = %q, want the FIRST one kept", meta.sessionID)
	}
	if meta.effectiveModel != "m" {
		t.Fatalf("the init capture regressed: effectiveModel = %q", meta.effectiveModel)
	}
}

// The class this change walks through. OnTurnFinished fired on "a session
// id exists", which was a PROXY for "the delegation succeeded" — sound only
// while a failure could not carry an id, which is precisely the fact being
// changed. The gate now says what it means.
func TestTurnFinishedDoesNotFireForATurnThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantFired bool
	}{
		{"a turn that finished", nil, true},
		{"a turn that failed, now carrying a session", errors.New("session ended without result"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The delegate's OWN condition, not a copy of it: a test that
			// re-spelled the rule here would stay green while the rule it
			// claims to pin was deleted.
			if got := turnFinished(tc.err, Result{SessionID: "s1"}); got != tc.wantFired {
				t.Errorf("turnFinished = %v, want %v", got, tc.wantFired)
			}
			// And the half that was there before: no session, nothing to
			// announce, whatever the outcome.
			if turnFinished(tc.err, Result{}) {
				t.Error("announced a turn for a delegation that never opened a session")
			}
		})
	}
}

// The ask_user pause is the path that PERSISTS a session across a human
// gate (ADR-089), and it reaches its builder with rm == nil as the RULE:
// the branch returns before the stream-error test because the hook firing
// is what cancels the stream, so no ResultMessage arrives. Until the
// streamed id existed, that path published an ANONYMOUS session — and
// packLiveSession, gated on a non-empty id, never ran on the one path it
// was written for.
//
// Pinned because the premise is easy to get backwards: a later editor who
// believes "the pause always has an rm" is free to move or drop the
// session-meta application, and the hole reopens on exactly the path that
// persists.
func TestAskUserPauseNamesItsSessionWithoutAResultMessage(t *testing.T) {
	b := sessionFailureBackend()
	task := Task{NodeID: "n", Iteration: 1}
	p := pendingAskUser{Question: "which one?"}

	res := b.buildAskUserPendingResult(task, p, nil, nil,
		sessionMeta{sessionID: "sess-from-init"}, "fp", time.Second, "")
	if res.SessionID != "sess-from-init" {
		t.Fatalf("SessionID = %q, want the streamed id — a pause that cannot name its session persists an anonymous one", res.SessionID)
	}
	if _, ok := res.Output["_needs_interaction"]; !ok {
		t.Fatalf("the pause envelope was lost: %v", res.Output)
	}
}
