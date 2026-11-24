package runtime

import (
	"context"
	"testing"
	"time"
)

type privateContextKey struct{}
type privateContextDrain struct {
	drainFakeRun
	ctx context.Context
	err error
}

func (r *privateContextDrain) ExportRunFiles(ctx context.Context) error {
	r.ctx = ctx
	r.err = ctx.Err()
	return nil
}
func TestRunFilesGrantCleanupKeepsIdentityWithoutCancellation(t *testing.T) {
	original, cancel := context.WithCancel(context.WithValue(context.Background(), privateContextKey{}, "tenant-owner-birth"))
	cancel()
	r := &privateContextDrain{}
	drainRunFilesOnCleanup(original, r, nil, nil)
	if r.err != nil {
		t.Fatalf("cleanup inherited cancellation: %v", r.err)
	}
	if r.ctx == nil || r.ctx.Value(privateContextKey{}) != "tenant-owner-birth" {
		t.Fatal("identity lost")
	}
	deadline, ok := r.ctx.Deadline()
	if !ok || time.Until(deadline) > sandboxRunFilesDrainTimeout || time.Until(deadline) < time.Minute {
		t.Fatal("cleanup budget", deadline)
	}
}
