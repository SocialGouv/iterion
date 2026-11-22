package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

type testCodemodeState map[string]json.RawMessage

func (b testCodemodeState) StoreState(_ context.Context, k string, v json.RawMessage) error {
	b[k] = v
	return nil
}
func (b testCodemodeState) LoadState(_ context.Context, k string) (json.RawMessage, bool, error) {
	v, ok := b[k]
	return v, ok, nil
}

func TestCodemodeStateReachesSeparateScriptChildren(t *testing.T) {
	bank := testCodemodeState{}
	run := func(bank CodemodeState, source string) string {
		t.Helper()
		opts := GenerationOptions{codemodeState: bank}
		if err := installCodemodeOn(&opts, childHost(), false, &scripthost.Limits{Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
		_, ctx := newToolDispatcher(t.Context(), nil, nil, nil, nil, nil, nil)
		payload, err := json.Marshal(map[string]string{"source": source})
		if err != nil {
			t.Fatal(err)
		}
		got, err := opts.Tools[0].Execute(ctx, payload)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	run(bank, `call.store("item", null); true`)
	if got := run(bank, `call.load("item") === null`); got != "true" {
		t.Fatalf("the next script child lost a stored null: %q", got)
	}
	if got := run(testCodemodeState{}, `call.load("item") === undefined`); got != "true" {
		t.Fatalf("another bank did not report a miss: %q", got)
	}
	if got := run(nil, `try { call.load("item"); false } catch (e) { e.code === "state_unavailable" }`); got != "true" {
		t.Fatalf("absent authority did not raise the typed refusal: %q", got)
	}
}

func TestCodemodeStateLauncherRejectsLegacyRunnerBeforeTask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	dir := t.TempDir()
	// A legacy bootstrap accepts only its known pre-task envelopes and task.
	// It must see the newer marker before it starts a model turn.
	s := &scriptedSandbox{script: `IFS= read -r line || exit 1
case "$line" in
  '{"type":"codemode_state_v1"}') printf '%s\n' '{"type":"result","data":{"error":"legacy runner: unexpected codemode_state_v1 before task"}}';;
  *) printf '%s\n' '{"type":"result","data":{}}';;
esac`}
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	ctx := WithCodemodeState(t.Context(), testCodemodeState{})
	_, err := b.Execute(ctx, delegate.Task{NodeID: "state", Model: "test/test-model", Codemode: true, WorkDir: dir, Sandbox: s})
	if err == nil || (!strings.Contains(err.Error(), "codemode_state_v1") && !strings.Contains(err.Error(), "runner must support codemode state v1")) {
		t.Fatalf("new launcher did not refuse a legacy runner explicitly: %v", err)
	}
}

func TestCodemodeStateLauncherRelayCapturesOnlyItsOwnBank(t *testing.T) {
	a, b := testCodemodeState{}, testCodemodeState{}
	if relay := codemodeStateRelay(WithCodemodeState(t.Context(), a), false); relay != nil {
		t.Fatal("codemode off advertised the bank")
	}
	relay := codemodeStateRelay(WithCodemodeState(t.Context(), a), true)
	ctx := WithCodemodeState(t.Context(), b)
	reply, err := relay(ctx, delegate.StateRequest{Op: delegate.StateStore, Key: "item", Value: []byte(`null`)})
	if err != nil || reply.Code != delegate.StateOK || string(a["item"]) != "null" || len(b) != 0 {
		t.Fatalf("launcher did not use its captured bank: code=%q err=%v", reply.Code, err)
	}
}
