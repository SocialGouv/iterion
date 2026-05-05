package blob

import (
	"fmt"
	"strings"
	"testing"
)

func TestArtifactKey(t *testing.T) {
	got := ArtifactKey("run_42", "agent_a", 3)
	want := "artifacts/run_42/agent_a/3.json"
	if got != want {
		t.Errorf("ArtifactKey: got %q want %q", got, want)
	}
}

func TestArtifactKey_PanicsOnEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty run_id")
		}
	}()
	_ = ArtifactKey("", "node", 1)
}

func TestArtifactKey_PanicsOnTraversal(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on '..' in node_id")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "node_id") {
			t.Errorf("panic should mention node_id: %v", r)
		}
	}()
	_ = ArtifactKey("run_1", "..", 1)
}

func TestArtifactKey_PanicsOnSeparator(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on '/' in component")
		}
	}()
	_ = ArtifactKey("run_1", "node/with-slash", 1)
}

func TestArtifactKey_PanicsOnNegativeVersion(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on negative version")
		}
	}()
	_ = ArtifactKey("run_1", "node", -1)
}
