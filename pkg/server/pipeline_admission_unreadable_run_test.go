package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// RVA-T9 R4: the two authorities that answer "may I mint a run for this
// card?" fail in OPPOSITE directions on the same unreadable run record.
//
//	dispatcher.lastRunHoldBeforeClaim → HOLD (fail closed, "no information
//	                                   is not no run")
//	server.pipelineTicketLaunchable   → LAUNCH (fail open)
//
// A transiently unreadable run store therefore makes the pipelines
// admission loop mint a sibling for a card whose run is alive.
func TestRVAT9_PipelineTicketLaunchableFailsOpenOnAnUnreadableRun(t *testing.T) {
	dir := t.TempDir()
	rs, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// A run record that EXISTS but cannot be decoded — the "store blipped /
	// half-written run.json" case, distinct from a pruned run.
	runDir := filepath.Join(dir, "runs", "run-broken")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := rs.LoadRun(context.Background(), "run-broken"); err == nil {
		t.Fatal("precondition: the run record must be unreadable")
	} else if err == store.ErrRunNotFound {
		t.Fatalf("precondition: want a decode error, not ErrRunNotFound: %v", err)
	} else {
		t.Logf("LoadRun error (not ErrRunNotFound): %v", err)
	}

	iss := &native.Issue{ID: "card-1", Title: "c", State: native.StateReady, Bot: "feature-dev", LastRunID: "run-broken"}
	if pipelineTicketLaunchable(context.Background(), rs, iss) {
		t.Fatalf("R4 REPRODUCED: pipelineTicketLaunchable said YES for a card whose run record could not be read — " +
			"the dispatcher's lastRunHoldBeforeClaim HOLDS on exactly this input")
	}
}
