package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The checkpoint survives every status transition (ADR-095), so a
// FINISHED run keeps whatever interaction pointer it held. Inspect must
// not report "Paused at" + a live "Interaction" id on a terminated run.
func TestInspectHidesStalePauseOnFinishedRun(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-stale", "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "gate", InteractionID: "I1"}
	if err := s.PauseRun(ctx, "r-stale", cp); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, "r-stale", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	p := &Printer{Format: OutputHuman, W: &buf}
	if err := RunInspect(InspectOptions{RunID: "r-stale", StoreDir: dir}, p); err != nil {
		t.Fatalf("RunInspect: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "Paused at") || strings.Contains(out, "I1") {
		t.Errorf("inspect of a FINISHED run advertises a pause:\n%s", out)
	}
}

// The middle of the matrix: a failed_resumable run keeps its anchor
// (where the post-mortem starts) but must not advertise a pause — this
// is the line that distinguishes the NodeID gate from an IsPaused one.
func TestInspectShowsAnchorWithoutPauseOnFailedResumable(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-parked", "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "implement", InteractionID: "I1"}
	if err := s.PauseRun(ctx, "r-parked", cp); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, "r-parked", store.RunStatusFailedResumable, "parked"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	p := &Printer{Format: OutputHuman, W: &buf}
	if err := RunInspect(InspectOptions{RunID: "r-parked", StoreDir: dir}, p); err != nil {
		t.Fatalf("RunInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "implement") {
		t.Errorf("inspect of a parked run lost its checkpoint anchor:\n%s", out)
	}
	if strings.Contains(out, "Paused at") || strings.Contains(out, "I1") {
		t.Errorf("inspect of a parked run advertises a pause:\n%s", out)
	}
}

// On a genuinely paused run the block stays: the pointer is live.
func TestInspectShowsPausePointerOnPausedRun(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-paused", "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "gate", InteractionID: "I1"}
	if err := s.PauseRun(ctx, "r-paused", cp); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	p := &Printer{Format: OutputHuman, W: &buf}
	if err := RunInspect(InspectOptions{RunID: "r-paused", StoreDir: dir}, p); err != nil {
		t.Fatalf("RunInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Paused at") || !strings.Contains(out, "I1") {
		t.Errorf("inspect of a PAUSED run lost its pause pointer:\n%s", out)
	}
}

func TestInspectSummarizesNativeRunWithoutRawState(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const id = "pc1_inspect_native"
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	if _, err := s.CreateRun(ctx, id, "deliver", nil); err != nil {
		t.Fatal(err)
	}
	identity := store.PortExecutionIdentity{Source: "private-source-identity", Graph: "private-graph-identity", Contract: "contract", Policy: "policy", Inputs: "inputs"}
	state := &store.PortExecution{Version: store.PortExecutionVersion, Revision: 1, Generation: 1, RootRunID: id, Identity: identity,
		Invocations: map[string]*store.PortInvocation{"produce": {ID: "produce", Node: "produce", Attempt: 1, Status: store.PortPending,
			Identity: store.PortExecutionIdentity{Source: "private-implementation-identity", Contract: "contract", Policy: "policy"}}},
		Collections: map[string]*store.PortCollection{"render": {ID: "render", Node: "render", Complete: true}},
		Products:    []string{"report"}}
	if err := store.SavePortExecution(ctx, s, id, 0, state); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := RunInspect(InspectOptions{RunID: id, StoreDir: dir}, &Printer{Format: OutputHuman, W: &buf}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, required := range []string{"ports-v1", "Native graph", "produce", "1 pending", "render", "map 0 items complete", "Product report", "waiting", "reported"} {
		if !strings.Contains(out, required) {
			t.Fatalf("native summary omitted %q:\n%s", required, out)
		}
	}
	if strings.Contains(out, "private-source-identity") || strings.Contains(out, "private-implementation-identity") {
		t.Fatalf("human summary exposed technical identities:\n%s", out)
	}
}
