package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type independentRememberContext struct {
	NodeExecutor
	captured context.Context
	fail     bool
}

func (s *independentRememberContext) Execute(ctx context.Context, n ir.Node, in map[string]any) (map[string]any, error) {
	s.captured = ctx
	if s.fail {
		s.fail = false
		return nil, errors.New("scripted first node failure")
	}
	return s.NodeExecutor.Execute(ctx, n, in)
}
func TestPreparedContextDoesNotFenceLaterResume(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.PrepareRunInvocation(t.Context(), fs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run", Status: store.RunStatusQueued}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	first := admissionSnapshotEngine(t, fs, nil, &calls)
	capture := &independentRememberContext{NodeExecutor: first.executor, fail: true}
	first.executor = capture
	err = first.RunPrepared(p.Context, "run", nil, p)
	row, loadErr := fs.LoadRun(t.Context(), "run")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	t.Logf("first=%v status=%s checkpoint=%v captured=%v", err, row.Status, row.Checkpoint != nil, capture.captured != nil)
	if err == nil || capture.captured == nil || row.Status != store.RunStatusFailedResumable {
		t.Fatal("fixture did not stop at real node")
	}
	next := admissionSnapshotEngine(t, fs, nil, &calls)
	err = next.Resume(capture.captured, "run", nil)
	row, loadErr = fs.LoadRun(t.Context(), "run")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	t.Logf("resume=%v status=%s nodes=%d", err, row.Status, calls)
	if err != nil || row.Status != store.RunStatusFinished || calls != 1 {
		t.Fatalf("PREPARED_GRANT_ESCAPED_ADMISSION: %v status=%s nodes=%d", err, row.Status, calls)
	}
}
