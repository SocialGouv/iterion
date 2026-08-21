package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestTakeSessionStateBlobStripsKeys(t *testing.T) {
	out := map[string]any{
		delegate.SessionStateBlobKey: []byte("blob"),
		delegate.SessionStateKey:     []byte("x"),
		"_session_state_ref":         "should-go",
		"keep":                       true,
	}
	got := takeSessionStateBlob(out)
	if string(got) != "blob" {
		t.Fatalf("got %q", got)
	}
	if _, ok := out[delegate.SessionStateBlobKey]; ok {
		t.Fatal("blob key still present")
	}
	if _, ok := out["_session_state_ref"]; ok {
		t.Fatal("ref leaked")
	}
	if _, ok := out["keep"]; !ok {
		t.Fatal("unrelated key stripped")
	}
}

func TestVisit1EligibleSourceRequiresOutputStamp(t *testing.T) {
	wf := &ir.Workflow{
		Edges: []*ir.Edge{
			{From: "src", To: "dst", With: []*ir.DataMapping{{Key: delegate.SessionIDKey, Raw: "sess-other"}}},
		},
	}
	rs := &runState{outputs: map[string]map[string]any{
		"src": {delegate.SessionIDKey: "sess-real", delegate.BackendNameKey: "claude_code"},
	}}
	if _, ok := visit1EligibleSource(wf, rs, "dst", "sess-other", "claude_code"); ok {
		t.Fatal("literal mapping must not count as provenance")
	}
	if src, ok := visit1EligibleSource(wf, rs, "dst", "sess-real", "claude_code"); !ok || src != "src" {
		t.Fatalf("stamp match: src=%q ok=%v", src, ok)
	}
}

func TestVisit1EligibleSourceTwoStampsFresh(t *testing.T) {
	wf := &ir.Workflow{
		Edges: []*ir.Edge{
			{From: "a", To: "dst"},
			{From: "b", To: "dst"},
		},
	}
	rs := &runState{outputs: map[string]map[string]any{
		"a": {delegate.SessionIDKey: "sess", delegate.BackendNameKey: "claude_code"},
		"b": {delegate.SessionIDKey: "sess", delegate.BackendNameKey: "claude_code"},
	}}
	if _, ok := visit1EligibleSource(wf, rs, "dst", "sess", "claude_code"); ok {
		t.Fatal("two stamp-eligible sources must be fail-closed")
	}
}

func TestCloneNodeSessions(t *testing.T) {
	src := map[string]store.NodeSessionSlot{"n": {SessionID: "s", StateRef: "r"}}
	dst := cloneNodeSessions(src)
	dst["n"] = store.NodeSessionSlot{SessionID: "other"}
	if src["n"].SessionID != "s" {
		t.Fatal("clone aliased")
	}
}

func persistWriterLoopWorkflow() *ir.Workflow {
	sidMap := func(from string) []*ir.DataMapping {
		return []*ir.DataMapping{{
			Key:  delegate.SessionIDKey,
			Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{from, delegate.SessionIDKey}, Raw: "{{outputs." + from + "._session_id}}"}},
			Raw:  "{{outputs." + from + "._session_id}}",
		}}
	}
	return &ir.Workflow{
		Name:  "persist_loop",
		Entry: "writer",
		Nodes: map[string]ir.Node{
			"writer": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "writer"}, Session: ir.SessionPersist},
			"judge":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "judge"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "writer", To: "judge"},
			{From: "judge", To: "done", Condition: "approved"},
			{From: "judge", To: "writer", LoopName: "fix", With: sidMap("judge")},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"fix": {Name: "fix", MaxIterations: 5, Entries: map[string]bool{"writer": true}, Body: map[string]bool{"writer": true, "judge": true}},
		},
	}
}

func TestPersistVisit1FreshVisit2OwnSlot(t *testing.T) {
	wf := persistWriterLoopWorkflow()
	var seen []string
	visits := 0
	exec := newStubExecutor()
	exec.on("writer", func(in map[string]any) (map[string]any, error) {
		visits++
		sid, _ := in[delegate.SessionIDKey].(string)
		seen = append(seen, sid)
		return map[string]any{
			delegate.SessionIDKey:        "sess-writer",
			delegate.BackendNameKey:      "claude_code",
			delegate.SessionStateBlobKey: []byte("pack:sess-writer"),
			"text":                       "draft",
			"_tokens":                    1,
		}, nil
	})
	exec.on("judge", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"approved":              visits >= 2,
			delegate.SessionIDKey:   "sess-judge",
			delegate.BackendNameKey: "claude_code",
		}, nil
	})

	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "run-persist-loop", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("writer visits=%d want 2 (%v)", len(seen), seen)
	}
	if seen[0] != "" {
		t.Errorf("visit 1 want fresh, got %q", seen[0])
	}
	if seen[1] != "sess-writer" {
		t.Errorf("visit 2 want own slot sess-writer (not inbound sess-judge), got %q", seen[1])
	}

	r, err := s.LoadRun(context.Background(), "run-persist-loop")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint != nil {
		if _, ok := r.Checkpoint.Outputs["writer"]["_session_state_ref"]; ok {
			t.Fatal("outputs must not carry _session_state_ref")
		}
		if _, ok := r.Checkpoint.Outputs["writer"][delegate.SessionStateBlobKey]; ok {
			t.Fatal("outputs must not carry packed blob")
		}
	}
}

func TestPersistWipedDirBetweenVisitsRunsFresh(t *testing.T) {
	wf := persistWriterLoopWorkflow()
	var seen []string
	visits := 0
	exec := newStubExecutor()
	s := tmpStore(t)
	exec.on("writer", func(in map[string]any) (map[string]any, error) {
		visits++
		sid, _ := in[delegate.SessionIDKey].(string)
		seen = append(seen, sid)
		return map[string]any{
			delegate.SessionIDKey:        "sess-writer",
			delegate.BackendNameKey:      "claude_code",
			delegate.SessionStateBlobKey: []byte("pack:sess-writer"),
			"text":                       "draft",
		}, nil
	})
	exec.on("judge", func(map[string]any) (map[string]any, error) {
		if visits == 1 {
			if r, err := s.LoadRun(context.Background(), "run-persist-wipe"); err == nil && r.Checkpoint != nil {
				for _, slot := range r.Checkpoint.NodeSessions {
					_ = store.AsBackendSessionStore(s).DeleteBackendSession(context.Background(), "run-persist-wipe", slot.StateRef)
				}
			}
		}
		return map[string]any{"approved": visits >= 2}, nil
	})
	if err := New(wf, s, exec).Run(context.Background(), "run-persist-wipe", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) < 2 {
		t.Fatalf("visits=%v", seen)
	}
	if seen[1] != "" {
		t.Errorf("wiped slot must run fresh, got %q", seen[1])
	}
}

func TestPersistLiteralInboundWithHasSessionIsFresh(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "literal_inbound",
		Entry: "src",
		Nodes: map[string]ir.Node{
			"src":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "src"}},
			"dst":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dst"}, Session: ir.SessionPersist},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "src", To: "dst", With: []*ir.DataMapping{{Key: delegate.SessionIDKey, Raw: "sess-other"}}},
			{From: "dst", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	var got string
	exec := newStubExecutor()
	exec.hasSessionFn = func(_, id string) bool { return id == "sess-other" }
	exec.on("src", func(map[string]any) (map[string]any, error) {
		return map[string]any{delegate.SessionIDKey: "sess-real", delegate.BackendNameKey: "claude_code"}, nil
	})
	exec.on("dst", func(in map[string]any) (map[string]any, error) {
		got, _ = in[delegate.SessionIDKey].(string)
		return map[string]any{delegate.SessionIDKey: "sess-dst", delegate.BackendNameKey: "claude_code"}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-literal", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "" {
		t.Errorf("literal sess-other with HasSession must not resume, got %q", got)
	}
}

func TestPersistHasSessionFingerprintFromSource(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fp_src",
		Entry: "src",
		Nodes: map[string]ir.Node{
			"src":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "src"}},
			"dst":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dst"}, Session: ir.SessionInherit},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "src", To: "dst", With: []*ir.DataMapping{
				{Key: delegate.SessionIDKey, Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"src", delegate.SessionIDKey}}}, Raw: "{{outputs.src._session_id}}"},
				{Key: delegate.SessionFingerprintKey, Raw: "inbound-wrong"},
			}},
			{From: "dst", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	var fp string
	exec := newStubExecutor()
	exec.on("src", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			delegate.SessionIDKey:          "sess-real",
			delegate.BackendNameKey:        "claude_code",
			delegate.SessionFingerprintKey: "fp-from-src",
			delegate.SessionStateBlobKey:   []byte("pack:sess-real"),
		}, nil
	})
	exec.on("dst", func(in map[string]any) (map[string]any, error) {
		fp, _ = in[delegate.SessionFingerprintKey].(string)
		return map[string]any{"ok": true}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-fp", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fp != "fp-from-src" {
		t.Errorf("HasSession path fingerprint want source stamp, got %q", fp)
	}
}

func TestPersistHasSessionFalseNoSessionID(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "nohas",
		Entry: "src",
		Nodes: map[string]ir.Node{
			"src":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "src"}},
			"dst":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dst"}, Session: ir.SessionInherit},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "src", To: "dst", With: []*ir.DataMapping{
				{Key: delegate.SessionIDKey, Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"src", delegate.SessionIDKey}}}, Raw: "{{outputs.src._session_id}}"},
			}},
			{From: "dst", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	var got string
	exec := newStubExecutor()
	exec.hasSessionFn = func(string, string) bool { return false }
	exec.on("src", func(map[string]any) (map[string]any, error) {
		return map[string]any{delegate.SessionIDKey: "sess-real", delegate.BackendNameKey: "claude_code"}, nil
	})
	exec.on("dst", func(in map[string]any) (map[string]any, error) {
		got, _ = in[delegate.SessionIDKey].(string)
		return map[string]any{"ok": true}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-nohas", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "" {
		t.Errorf("HasSession false + no slot must strip SessionID, got %q", got)
	}
}

func TestPersistCapabilityFalseStrips(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "kimi",
		Entry: "src",
		Nodes: map[string]ir.Node{
			"src":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "src"}},
			"dst":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dst"}, Session: ir.SessionPersist},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "src", To: "dst", With: []*ir.DataMapping{
				{Key: delegate.SessionIDKey, Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"src", delegate.SessionIDKey}}}, Raw: "{{outputs.src._session_id}}"},
			}},
			{From: "dst", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	var got string
	exec := newStubExecutor()
	no := false
	exec.capOK = &no
	exec.capBackend = "kimi"
	exec.on("src", func(map[string]any) (map[string]any, error) {
		return map[string]any{delegate.SessionIDKey: "sess-real", delegate.BackendNameKey: "kimi"}, nil
	})
	exec.on("dst", func(in map[string]any) (map[string]any, error) {
		got, _ = in[delegate.SessionIDKey].(string)
		return map[string]any{"ok": true}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-kimi", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "" {
		t.Errorf("unsupported backend must strip, got %q", got)
	}
}

func TestPersistAskUserCarriesBlobNotOnCheckpoint(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Nodes["worker"] = &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "worker"},
		Session:           ir.SessionPersist,
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}
	exec := newStubExecutor()
	exec.on("worker", func(map[string]any) (map[string]any, error) {
		return nil, &model.ErrNeedsInteraction{
			NodeID:           "worker",
			Questions:        map[string]any{delegate.AskUserQuestionKey: "ok?"},
			SessionID:        "sess-ask",
			Backend:          "claude_code",
			SessionStateBlob: []byte("packed-cli"),
		}
	})
	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "run-ask-blob", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want paused, got %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-ask-blob")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.BackendSessionStateRef == "" {
		t.Fatal("pause checkpoint must carry BackendSessionStateRef")
	}
	if len(r.Checkpoint.NodeSessions) != 0 {
		t.Fatalf("in-flight ask_user must not write NodeSessions: %+v", r.Checkpoint.NodeSessions)
	}
	bss := store.AsBackendSessionStore(s)
	body, err := bss.GetBackendSession(context.Background(), "run-ask-blob", r.Checkpoint.BackendSessionStateRef)
	if err != nil {
		t.Fatalf("Get pause blob: %v", err)
	}
	if string(body) != "packed-cli" {
		t.Errorf("blob = %q", body)
	}
}

func TestPersistPauseRunErrorKeepsBlob(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	exec := newStubExecutor()
	exec.on("worker", func(map[string]any) (map[string]any, error) {
		return nil, &model.ErrNeedsInteraction{
			NodeID:           "worker",
			Questions:        map[string]any{delegate.AskUserQuestionKey: "ok?"},
			SessionID:        "sess-ask",
			Backend:          "claude_code",
			SessionStateBlob: []byte("keep-me"),
		}
	})
	inner := tmpStore(t)
	wrapped := &sessionStoreWrap{
		RunStore: inner,
		pauseRun: func(ctx context.Context, id string, cp *store.Checkpoint) error {
			if err := inner.PauseRun(ctx, id, cp); err != nil {
				return err
			}
			return errors.New("pause client timeout")
		},
	}
	err := New(wf, wrapped, exec).Run(context.Background(), "run-pause-err", nil)
	if err == nil {
		t.Fatal("expected pause error")
	}
	dir := filepath.Join(inner.Root(), "runs", "run-pause-err", "backend-sessions")
	ents, rdErr := os.ReadDir(dir)
	if rdErr != nil || len(ents) == 0 {
		t.Fatalf("blob must be kept after PauseRun error: dir=%v err=%v", ents, rdErr)
	}
}

func TestPersistNonPersistReInvokeNoNodeSessions(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	calls := 0
	exec := newStubExecutor()
	exec.on("worker", func(map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{
				NodeID:           "worker",
				Questions:        map[string]any{delegate.AskUserQuestionKey: "ok?"},
				SessionID:        "sess-ask",
				Backend:          "claude_code",
				SessionStateBlob: []byte("packed"),
			}
		}
		return map[string]any{"text": "done", "_tokens": 1}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-np-reinvoke", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want paused, got %v", err)
	}
	if err := eng.Resume(context.Background(), "run-np-reinvoke", map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-np-reinvoke")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint != nil && len(r.Checkpoint.NodeSessions) != 0 {
		t.Fatalf("non-persist reInvoke must not write NodeSessions: %+v", r.Checkpoint.NodeSessions)
	}
}

func TestPersistCheckpointFailKeepsNewSlot(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cpfail",
		Entry: "writer",
		Nodes: map[string]ir.Node{
			"writer": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "writer"}, Session: ir.SessionPersist},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "writer", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	exec := newStubExecutor()
	exec.on("writer", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			delegate.SessionIDKey:        "sess-new",
			delegate.BackendNameKey:      "claude_code",
			delegate.SessionStateBlobKey: []byte("pack:sess-new"),
			"text":                       "ok",
		}, nil
	})
	inner := tmpStore(t)
	saves := 0
	wrapped := &sessionStoreWrap{
		RunStore: inner,
		saveCheckpoint: func(ctx context.Context, id string, cp *store.Checkpoint) error {
			saves++
			if saves == 1 {
				return errors.New("checkpoint boom")
			}
			return inner.SaveCheckpoint(ctx, id, cp)
		},
	}
	err := New(wf, wrapped, exec).Run(context.Background(), "run-cpfail", nil)
	if err == nil {
		t.Fatal("expected fail-stop")
	}
	r, loadErr := inner.LoadRun(context.Background(), "run-cpfail")
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if r.Checkpoint == nil {
		t.Fatal("expected FailRunResumable checkpoint")
	}
	slot, ok := r.Checkpoint.NodeSessions["writer"]
	if !ok || slot.SessionID != "sess-new" || slot.StateRef == "" {
		t.Fatalf("fail-stop checkpoint must carry NEW slot, got %+v", r.Checkpoint.NodeSessions)
	}
}

func TestPersistPauseUnpackFailStrips(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Nodes["worker"] = &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "worker"},
		Session:           ir.SessionPersist,
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}
	calls := 0
	var secondSID string
	exec := newStubExecutor()
	exec.unpackErr = errors.New("corrupt pack")
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{
				NodeID:           "worker",
				Questions:        map[string]any{delegate.AskUserQuestionKey: "ok?"},
				SessionID:        "sess-ask",
				Backend:          "claude_code",
				SessionStateBlob: []byte("packed"),
			}
		}
		secondSID, _ = in[delegate.SessionIDKey].(string)
		return map[string]any{
			delegate.SessionIDKey:        "sess-ask",
			delegate.BackendNameKey:      "claude_code",
			delegate.SessionStateBlobKey: []byte("pack:sess-ask"),
			"text":                       "ok",
		}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-unpack-fail", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want paused, got %v", err)
	}
	if err := eng.Resume(context.Background(), "run-unpack-fail", map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if secondSID != "" {
		t.Errorf("unpack failure must strip SessionID, got %q", secondSID)
	}
}

func TestPersistFanOutRuntimeError(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fanout_persist",
		Entry: "r",
		Nodes: map[string]ir.Node{
			"r":    &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, RouterMode: ir.RouterFanOutAll},
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, Session: ir.SessionPersist, LLMFields: ir.LLMFields{Readonly: true}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}, LLMFields: ir.LLMFields{Readonly: true}},
			"join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "r", To: "a"},
			{From: "r", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	exec := newStubExecutor()
	exec.on("a", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	exec.on("b", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	exec.on("join", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-fanout-persist", nil)
	if err == nil {
		t.Fatal("expected persist-in-fan-out error")
	}
	if !strings.Contains(err.Error(), "session: persist") {
		t.Errorf("error should mention persist, got: %v", err)
	}
}

// sessionStoreWrap forwards RunStore and BackendSessionStore, with optional
// hooks for PauseRun / SaveCheckpoint fail-stop tests.
type sessionStoreWrap struct {
	store.RunStore
	saveCheckpoint func(ctx context.Context, id string, cp *store.Checkpoint) error
	pauseRun       func(ctx context.Context, id string, cp *store.Checkpoint) error
}

func (w *sessionStoreWrap) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	if w.saveCheckpoint != nil {
		return w.saveCheckpoint(ctx, id, cp)
	}
	return w.RunStore.SaveCheckpoint(ctx, id, cp)
}

func (w *sessionStoreWrap) PauseRun(ctx context.Context, id string, cp *store.Checkpoint) error {
	if w.pauseRun != nil {
		return w.pauseRun(ctx, id, cp)
	}
	return w.RunStore.PauseRun(ctx, id, cp)
}

func (w *sessionStoreWrap) PutBackendSession(ctx context.Context, runID, ref string, body []byte) error {
	return store.AsBackendSessionStore(w.RunStore).PutBackendSession(ctx, runID, ref, body)
}
func (w *sessionStoreWrap) GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error) {
	return store.AsBackendSessionStore(w.RunStore).GetBackendSession(ctx, runID, ref)
}
func (w *sessionStoreWrap) DeleteBackendSession(ctx context.Context, runID, ref string) error {
	return store.AsBackendSessionStore(w.RunStore).DeleteBackendSession(ctx, runID, ref)
}
