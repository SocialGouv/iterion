package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// exportingRun fakes a copy-based driver's Run (kubernetes shape):
// it implements sandbox.WorkspaceExporter and records the call.
type exportingRun struct {
	sandbox.Run
	calls int
	err   error
}

func (f *exportingRun) Driver() string { return "fake-k8s" }
func (f *exportingRun) ExportWorkspace(context.Context) error {
	f.calls++
	return f.err
}

// plainRun has no exporter capability (docker/noop shape).
type plainRun struct{ sandbox.Run }

func (plainRun) Driver() string { return "plain" }

// TestExportSandboxWorkspaceOnCleanup pins the write-back contract: an
// exporting driver is exported exactly once with no event on success; a
// failure emits the visible sandbox_workspace_export_failed event (the
// run's un-pushed work is about to be destroyed with the pod — never
// silent); a driver without the capability is untouched.
func TestExportSandboxWorkspaceOnCleanup(t *testing.T) {
	var events []store.EventType
	var payloads []map[string]any
	emit := func(tp store.EventType, data map[string]any) error {
		events = append(events, tp)
		payloads = append(payloads, data)
		return nil
	}

	ok := &exportingRun{}
	exportSandboxWorkspaceOnCleanup(ok, nil, emit)
	if ok.calls != 1 {
		t.Fatalf("export calls = %d, want 1", ok.calls)
	}
	if len(events) != 0 {
		t.Fatalf("no event expected on success, got %v", events)
	}

	bad := &exportingRun{err: errors.New("kubectl exec: pod gone")}
	exportSandboxWorkspaceOnCleanup(bad, nil, emit)
	if bad.calls != 1 {
		t.Fatalf("export calls = %d, want 1", bad.calls)
	}
	if len(events) != 1 || events[0] != store.EventSandboxWorkspaceExportFailed {
		t.Fatalf("expected the export-failed event, got %v", events)
	}
	if payloads[0]["driver"] != "fake-k8s" || payloads[0]["error"] != "kubectl exec: pod gone" {
		t.Fatalf("event payload = %+v", payloads[0])
	}

	// A driver without the capability (docker bind mount / noop host
	// passthrough) is a clean no-op.
	exportSandboxWorkspaceOnCleanup(plainRun{}, nil, emit)
	if len(events) != 1 {
		t.Fatalf("non-exporter must be a no-op, got events %v", events)
	}
}
