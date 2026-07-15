package runtime

import "testing"

// TestRunEvents_Sticky verifies the run-scoped registry's two delivery orders:
// a wait that arrives AFTER the emit still observes it (sticky), and a wait that
// arrives BEFORE the emit blocks until the signal.
func TestRunEvents_Sticky(t *testing.T) {
	re := newRunEvents()

	// emit-then-wait: the channel is already closed, payload is available.
	re.signal("ready", map[string]any{"value": 42})
	select {
	case <-re.waitChan("ready"):
	default:
		t.Fatal("waitChan for an already-fired event must be closed (sticky)")
	}
	if got := re.payloadFor("ready"); got["value"] != 42 {
		t.Errorf("payload value = %v, want 42", got["value"])
	}

	// wait-then-emit: the channel is open until signal closes it.
	ch := re.waitChan("later")
	select {
	case <-ch:
		t.Fatal("waitChan for an un-fired event must block")
	default:
	}
	re.signal("later", map[string]any{"value": 7})
	select {
	case <-ch:
	default:
		t.Fatal("waitChan must be closed after signal")
	}
	if got := re.payloadFor("later"); got["value"] != 7 {
		t.Errorf("payload value = %v, want 7", got["value"])
	}
}

// TestRunEvents_PayloadDeepIsolation verifies the ADR-051 immutability boundary
// holds for NESTED payload values: a waiter that mutates a nested map/slice in
// the payload it received must not corrupt the registry's stored event (nor any
// other waiter's copy). This is the regression for the shallow-clone bug — a
// per-key copy would leave the nested map aliased and let the mutation leak back.
func TestRunEvents_PayloadDeepIsolation(t *testing.T) {
	re := newRunEvents()
	re.signal("nested", map[string]any{
		"meta": map[string]any{"count": 1},
		"tags": []any{"a", "b"},
	})

	// First waiter reads the payload and mutates the nested structures.
	first := re.payloadFor("nested")
	first["meta"].(map[string]any)["count"] = 999
	first["tags"].([]any)[0] = "MUTATED"

	// A second, independent read must still see the original nested values.
	second := re.payloadFor("nested")
	if got := second["meta"].(map[string]any)["count"]; got != 1 {
		t.Errorf("nested map leaked mutation: count = %v, want 1", got)
	}
	if got := second["tags"].([]any)[0]; got != "a" {
		t.Errorf("nested slice leaked mutation: tags[0] = %v, want \"a\"", got)
	}
}

// TestRunEvents_DoubleSignal verifies a second emit of the same event does not
// panic on a re-close and refreshes the payload.
func TestRunEvents_DoubleSignal(t *testing.T) {
	re := newRunEvents()
	re.signal("e", map[string]any{"n": 1})
	re.signal("e", map[string]any{"n": 2}) // must not panic on re-close
	if got := re.payloadFor("e"); got["n"] != 2 {
		t.Errorf("payload n = %v, want 2 (latest)", got["n"])
	}
}
