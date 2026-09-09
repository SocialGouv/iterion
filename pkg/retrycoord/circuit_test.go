package retrycoord

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestKeyPrefersWorkflowRevision(t *testing.T) {
	got := Key(&store.Run{WorkflowHash: "abc", WorkflowName: "demo"})
	if got != "workflow:abc" {
		t.Fatalf("key = %q", got)
	}
	if got := Key(&store.Run{WorkflowName: "demo"}); got != "workflow-name:demo" {
		t.Fatalf("legacy key = %q", got)
	}
}

func TestCircuitConfigFromEnv(t *testing.T) {
	t.Setenv(EnvThreshold, "4")
	t.Setenv(EnvCooldown, "2m")
	c := FromEnv()
	if c.Threshold != 4 || c.Cooldown != 2*time.Minute {
		t.Fatalf("config = %+v", c)
	}
}
