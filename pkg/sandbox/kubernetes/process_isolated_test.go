package kubernetes

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestRun_isProcessIsolated: a pod never shares the node's process
// namespace, so a signal to every process from one of its commands reaches
// only the pod's.
func TestRun_isProcessIsolated(t *testing.T) {
	var run sandbox.Run = &Run{}
	if pi, ok := run.(sandbox.ProcessIsolated); !ok || !pi.ProcessIsolated() {
		t.Fatal("a kubernetes Run does not declare the process namespace of its own it has")
	}
}
