package model

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// captureEmitter records the events a store hook appends.
type captureEmitter struct{ events []store.Event }

func (c *captureEmitter) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	c.events = append(c.events, evt)
	return &evt, nil
}

// The executor bridge: a delegate's lifecycle step reaches the executor's
// hook with the node it happened on. A bridge that is not wired leaves the
// delegate's own tests green while no event is ever persisted.
func TestDelegateHooksBridgeTheBackgroundWorkLifecycle(t *testing.T) {
	var gotNode string
	var got BackgroundWorkInfo
	e := newFallbackExecutor(delegate.NewRegistry(), EventHooks{
		OnBackgroundWork: func(nodeID string, info BackgroundWorkInfo) { gotNode, got = nodeID, info },
	})
	h := e.delegateHooksFor("campaign", delegate.BackendClaudeCode, 0)
	if h.OnBackgroundWork == nil {
		t.Fatal("the delegate hooks carry no OnBackgroundWork: lifecycle steps are dropped")
	}
	h.OnBackgroundWork(delegate.BackgroundWork{Backend: "claude_code", Phase: delegate.BackgroundAbandoned,
		Running: 2, Tasks: []string{"a", "b"}, WaitedFor: 3 * time.Second, Reason: "budget"})
	want := BackgroundWorkInfo{Backend: "claude_code", Phase: "abandoned", Running: 2, Tasks: []string{"a", "b"}, WaitedFor: 3 * time.Second, Reason: "budget"}
	if gotNode != "campaign" || !reflect.DeepEqual(got, want) {
		t.Fatalf("bridged = %q %+v, want campaign %+v", gotNode, got, want)
	}
}

func TestStoreHooksPersistTheBackgroundWorkLifecycle(t *testing.T) {
	em := &captureEmitter{}
	hooks := NewStoreEventHooks(context.Background(), em, "run-bg", iterlog.Nop(), nil, nil)
	if hooks.OnBackgroundWork == nil {
		t.Fatal("the store hooks do not register OnBackgroundWork")
	}
	hooks.OnBackgroundWork("campaign", BackgroundWorkInfo{Backend: "claude_code", Phase: "abandoned", Running: 2,
		Tasks: []string{"auditor (local_agent, t1)", "checker (local_workflow, w2)"}, WaitedFor: 1500 * time.Millisecond, Reason: "budget"})
	var found *store.Event
	for i := range em.events {
		if em.events[i].Type == store.EventDelegateBackground {
			found = &em.events[i]
		}
	}
	if found == nil {
		t.Fatalf("no delegate_background event among %d events", len(em.events))
	}
	if found.NodeID != "campaign" || found.Data["phase"] != "abandoned" || found.Data["backend"] != "claude_code" {
		t.Fatalf("event = %+v", found)
	}
	// The labels are what an operator reads to know WHICH work was lost.
	want := map[string]any{"running": 2, "tasks": []string{"auditor (local_agent, t1)", "checker (local_workflow, w2)"},
		"waited_ms": int64(1500), "reason": "budget"}
	for k, v := range want {
		if !reflect.DeepEqual(found.Data[k], v) {
			t.Fatalf("data[%q] = %#v, want %#v (event %+v)", k, found.Data[k], v, found.Data)
		}
	}
}
