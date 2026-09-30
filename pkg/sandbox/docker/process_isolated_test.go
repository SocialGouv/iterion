package docker

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestRun_isProcessIsolated: a container never shares the host's process
// namespace, so a signal to every process from one of its commands reaches
// only the container's.
func TestRun_isProcessIsolated(t *testing.T) {
	var run sandbox.Run = &Run{}
	if pi, ok := run.(sandbox.ProcessIsolated); !ok || !pi.ProcessIsolated() {
		t.Fatal("a docker Run does not declare the process namespace of its own it has")
	}
}
