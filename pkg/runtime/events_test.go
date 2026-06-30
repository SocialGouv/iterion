package runtime

import "testing"

// TestRunEvents_Sticky verifies the run-scoped registry's two delivery orders:
// a wait that arrives AFTER the emit still observes it (sticky), and a wait that
// arrives BEFORE the emit blocks until the signal.
func TestRunEvents_Sticky(t *testing.T) {
	re := newRunEvents()

	// emit-then-wait: the channel is already closed, payload is available.
	re.signal("ready", map[string]interface{}{"value": 42})
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
	re.signal("later", map[string]interface{}{"value": 7})
	select {
	case <-ch:
	default:
		t.Fatal("waitChan must be closed after signal")
	}
	if got := re.payloadFor("later"); got["value"] != 7 {
		t.Errorf("payload value = %v, want 7", got["value"])
	}
}

// TestRunEvents_DoubleSignal verifies a second emit of the same event does not
// panic on a re-close and refreshes the payload.
func TestRunEvents_DoubleSignal(t *testing.T) {
	re := newRunEvents()
	re.signal("e", map[string]interface{}{"n": 1})
	re.signal("e", map[string]interface{}{"n": 2}) // must not panic on re-close
	if got := re.payloadFor("e"); got["n"] != 2 {
		t.Errorf("payload n = %v, want 2 (latest)", got["n"])
	}
}
