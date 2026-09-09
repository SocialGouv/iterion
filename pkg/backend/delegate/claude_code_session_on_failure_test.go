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
// announced on `system/init` — the id was logged and dropped.
//
// Scope, so the next reader does not over-read this: naming it is a
// REPORTING fix on this path. The id does reach the engine now — the
// executor hands a failed Result's metered output up rather than a bare
// nil, `_session_id` stamped on it — but nothing on the failure path reads
// it: commitPersistSlot, the only writer of a node's session slot, runs
// after a node SUCCEEDS. So the failure checkpoint still carries no backend
// session and nothing above the delegate resumes a dead node's. What is
// pinned here is the id's provenance and its precedence — the prerequisite
// for wiring that, not the wiring.
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
			t.Fatalf("SessionID = %q, want the id system/init announced — a failure that names no session leaves the run nothing to report or, later, to resume", res.SessionID)
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
// still names the session on whatever it did emit — so the capture takes
// any subtype. `init` stays the AUTHORITY though: it is the CLI announcing
// THIS session, and without that precedence a hook or sub-agent event
// reaching the stream first would pin its own id onto the checkpoint, and
// the resume would reopen the wrong conversation.
func TestSystemMessageCapturesTheSessionOnAnySubtypeButInitDecides(t *testing.T) {
	b := sessionFailureBackend()
	task := Task{NodeID: "n", Iteration: 1}

	var meta sessionMeta
	b.handleSystemMessage(&claudesdk.SystemMessage{Subtype: "hook", SessionID: "s-early"}, task, &meta)
	if meta.sessionID != "s-early" {
		t.Fatalf("sessionID = %q, want s-early — a non-init subtype names the session too", meta.sessionID)
	}
	b.handleSystemMessage(&claudesdk.SystemMessage{Subtype: "init", SessionID: "s-init", Model: "m"}, task, &meta)
	if meta.sessionID != "s-init" {
		t.Fatalf("sessionID = %q, want s-init — a provisional id yields to the CLI's own announcement", meta.sessionID)
	}
	if meta.effectiveModel != "m" {
		t.Fatalf("the init capture regressed: effectiveModel = %q", meta.effectiveModel)
	}

	// And once init has spoken, nothing displaces it: not a sub-agent's
	// system event, not a second init.
	b.handleSystemMessage(&claudesdk.SystemMessage{Subtype: "hook", SessionID: "s-sub"}, task, &meta)
	b.handleSystemMessage(&claudesdk.SystemMessage{Subtype: "init", SessionID: "s-other-init"}, task, &meta)
	if meta.sessionID != "s-init" {
		t.Fatalf("sessionID = %q, want s-init kept — the checkpoint must name the session this call opened", meta.sessionID)
	}
}

// The class this change walks through. OnTurnFinished fired on "a session
// id exists", which was a PROXY for "the CLI got far enough to have a
// turn" — sound only while a failure could not carry an id, which is
// precisely the fact being changed.
//
// The replacement has to keep the anchor a FAILED-but-completed turn used
// to leave. The hook's one consumer writes the TurnCheckpoint the Fork API
// needs, and a node that ran a whole session and ended on a rendered API
// error is exactly the one an operator forks to recover. Narrowing the gate
// to `err == nil` would have deleted that anchor silently, and the only
// symptom would be a 400 at fork time months later: "only agent/judge nodes
// that have completed at least one LLM turn can be forked".
func TestTurnFinishedAnnouncesEveryTurnTheCLICompleted(t *testing.T) {
	for _, tc := range []struct {
		name             string
		err              error
		cliTurnCompleted bool
		wantFired        bool
	}{
		{"a turn that finished", nil, true, true},
		{"a result the guards typed a failure — still a finished turn, still forkable",
			errors.New("delegate: claude-code error: subtype=error_during_execution"), true, true},
		{"the ask_user pause: no result message, but a deliberate nil error", nil, false, true},
		{"a stream that died before any result — no turn to announce",
			errors.New("session ended without result"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The delegate's OWN condition, not a copy of it: a test that
			// re-spelled the rule here would stay green while the rule it
			// claims to pin was deleted.
			if got := turnFinished(tc.err, tc.cliTurnCompleted, Result{SessionID: "s1"}); got != tc.wantFired {
				t.Errorf("turnFinished = %v, want %v", got, tc.wantFired)
			}
			// And the half that was there before: no session, nothing to
			// announce, whatever the outcome.
			if turnFinished(tc.err, tc.cliTurnCompleted, Result{}) {
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
	// The fingerprint rides with the id: the checkpoint needs it to be
	// ALLOWED to reuse the session it records — a fork whose parent
	// provider is unknown is dropped, so an anonymous id resumes nothing.
	if res.SessionFingerprint != "fp" {
		t.Fatalf("SessionFingerprint = %q, want fp — the id alone does not let a fork resume", res.SessionFingerprint)
	}

	// And the exception keeps rm's id, decided in the ONE place that
	// decides it. The pause used to spell that precedence a second time
	// inline, in a function whose premise is that rm is nil.
	withRM := b.buildAskUserPendingResult(task, p, nil,
		&claudesdk.ResultMessage{SessionID: "sess-from-result"},
		sessionMeta{sessionID: "sess-from-init"}, "fp", time.Second, "")
	if withRM.SessionID != "sess-from-result" {
		t.Fatalf("SessionID = %q, want the result message's", withRM.SessionID)
	}
}
