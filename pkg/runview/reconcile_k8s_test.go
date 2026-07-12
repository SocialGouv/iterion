package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestReconcileSandboxK8sResources_SafeNoOp pins that the k8s sandbox
// reaper is a harmless no-op off-cluster (kubernetes.Detect fails, we
// swallow it) and — crucially — returns immediately without an
// in-cluster probe when the store lacks cross-process-lock authority (the
// lock-less cloud server). Both cases must neither panic nor error.
func TestReconcileSandboxK8sResources_SafeNoOp(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	fs, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	t.Run("with lock authority", func(t *testing.T) {
		svc, err := NewService("", WithLogger(logger), WithStore(fs))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		defer svc.Stop(context.Background())
		svc.reconcileSandboxK8sResources() // off-cluster → Detect fails → no-op
	})

	t.Run("without lock authority", func(t *testing.T) {
		svc, err := NewService("", WithLogger(logger), WithStore(noLockAuthorityStore{fs}))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		defer svc.Stop(context.Background())
		// Must return at the authority gate, before any k8s probe.
		svc.reconcileSandboxK8sResources()
	})
}
