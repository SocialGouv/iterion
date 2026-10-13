package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// TestTheWorkerFamilyIsModelDrivenNotRefused draws the line the refused list
// rests on, from the production registry.
//
// `worker_create` answers `"status": "spawning"` and starts no subprocess —
// which reads like the `agent` stub this package refuses, and is not it. The
// worker registry is a state machine the MODEL drives: the node spawns the
// terminal itself (a `bash` tool call) and feeds back what it shows through
// `worker_observe`, which is what advances the machine. iterion wires the same
// executors against the same registry type claw's own conversation loop does
// (internal/runtime/conversation.go), so the family is SERVED, in claw's own
// contract, and `spawning` is a fact about the tracker rather than a start
// nobody made.
//
// A claw bump that made these executors answer without a state machine behind
// them would be the `agent` case, and would belong on the refused list — this
// test is what notices.
func TestTheWorkerFamilyIsModelDrivenNotRefused(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	call := func(name string, in map[string]any) map[string]any {
		t.Helper()
		td, resolveErr := reg.Resolve(name)
		if resolveErr != nil {
			t.Fatalf("resolve %s: %v", name, resolveErr)
		}
		raw, marshalErr := json.Marshal(in)
		if marshalErr != nil {
			t.Fatalf("marshal %s input: %v", name, marshalErr)
		}
		out, execErr := td.Execute(context.Background(), raw)
		if execErr != nil {
			t.Fatalf("%s: %v", name, execErr)
		}
		var got map[string]any
		if jsonErr := json.Unmarshal([]byte(out), &got); jsonErr != nil {
			t.Fatalf("%s returned non-JSON %q: %v", name, out, jsonErr)
		}
		return got
	}

	created := call("worker_create", map[string]any{"cwd": t.TempDir()})
	id, _ := created["worker_id"].(string)
	if id == "" {
		t.Fatalf("worker_create returned no worker_id: %v", created)
	}
	if created["status"] != "spawning" {
		t.Fatalf("worker_create status = %v, want \"spawning\" — the premise of this test changed", created["status"])
	}

	// The step the state machine turns on: screen text from the terminal the
	// node owns. Without it the worker stays `spawning` forever, which is a
	// caller that never observed, not a tool that cannot serve.
	observed := call("worker_observe", map[string]any{"worker_id": id, "screen_text": "claw > ready for input\n"})
	if observed["status"] != "ready_for_prompt" {
		t.Errorf("worker_observe left the worker at %v: the family would be a tracker nothing can advance, the state the refused list is for", observed["status"])
	}
	if ready := call("worker_await_ready", map[string]any{"worker_id": id}); ready["ready"] != true {
		t.Errorf("worker_await_ready = %v after an observed ready prompt", ready)
	}
	if sent := call("worker_send_prompt", map[string]any{"worker_id": id, "prompt": "do the thing"}); sent["status"] != "running" {
		t.Errorf("worker_send_prompt left the worker at %v, want \"running\"", sent["status"])
	}

	// And the distinction is recorded where it is read: the family is not on
	// the refused list, and `agent` is.
	for _, name := range []string{"worker_create", "worker_observe", "worker_send_prompt", "worker_await_ready"} {
		if _, refused := toolcatalog.ClawRefusesBuiltin(name); refused {
			t.Errorf("%q is on the refused list, yet this test just drove it through its state machine", name)
		}
	}
}
