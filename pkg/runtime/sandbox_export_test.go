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

// capturingRun fakes an export-based driver's Run that also reports the
// sandbox-side workspace HEAD (the kubernetes pair).
type capturingRun struct {
	exportingRun
	head string
	err  error
}

func (f *capturingRun) CaptureWorkspaceHead(context.Context) (string, error) {
	return f.head, f.err
}

// TestCaptureSandboxWorkspaceIntegrity pins the capture contract the
// banking invariant rests on: a captured HEAD is exposed as Applicable;
// a capture FAILURE is recorded as unverifiable (CaptureErr), never
// silently dropped; a workspace-less run and a non-capturing driver
// leave the zero value (nothing to verify).
func TestCaptureSandboxWorkspaceIntegrity(t *testing.T) {
	e := &Engine{}
	e.captureSandboxWorkspaceIntegrity(&capturingRun{head: "abc123"})
	if got := e.SandboxWorkspaceIntegrity(); !got.Applicable || got.PodHead != "abc123" || got.CaptureErr != "" {
		t.Fatalf("captured integrity = %+v, want Applicable with PodHead abc123", got)
	}

	e = &Engine{}
	e.captureSandboxWorkspaceIntegrity(&capturingRun{err: errors.New("pod gone")})
	if got := e.SandboxWorkspaceIntegrity(); !got.Applicable || got.PodHead != "" || got.CaptureErr != "pod gone" {
		t.Fatalf("failed capture integrity = %+v, want Applicable with CaptureErr", got)
	}

	e = &Engine{}
	e.captureSandboxWorkspaceIntegrity(&capturingRun{}) // ("", nil) = workspace-less
	if got := e.SandboxWorkspaceIntegrity(); got.Applicable {
		t.Fatalf("workspace-less run must stay non-applicable, got %+v", got)
	}

	e = &Engine{}
	e.captureSandboxWorkspaceIntegrity(plainRun{})
	if got := e.SandboxWorkspaceIntegrity(); got.Applicable {
		t.Fatalf("non-capturing driver must stay non-applicable, got %+v", got)
	}
}
