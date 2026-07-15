package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Parent: prep(tool) -> run_child(subbot) -> done. The child holds a HUMAN
// gate, so the in-process child engine pauses; the parent's subbot node must
// park (not fail), the answer must resume the CHILD run, and the parent must
// finish with the child's terminal output mapped through the subbot node.
//
// This is the studio-path regression for two gaps at once:
//   - the service's in-process engines had no SubbotRunner at all ("no
//     SubbotRunner is wired" on any subbot-bearing bot launched from the
//     studio);
//   - a human gate inside a child failed the parent instead of surfacing as
//     an answerable pipeline-board review.
const subbotGateChild = `
schema gate_out:
  approved: bool
  notes: string

schema child_out:
  verdict: string

human review:
  output: gate_out

compute wrap:
  output: child_out
  expr:
    verdict: "outputs.review.notes"

workflow gate_child:
  entry: review
  review -> wrap
  wrap -> done
`

const subbotGateParent = `
schema pout:
  ready: bool

schema child_out:
  verdict: string

tool prep:
  command: ` + "`printf '{\"ready\":true}'`" + `
  output: pout

subbot run_child:
  source: "gate_child.bot"
  output: child_out

## Consumes the subbot's output downstream — the functional guarantee that a
## child resumed EXTERNALLY (out of the parent engine's process/goroutine)
## still feeds {{outputs.run_child.*}} to the rest of the parent graph.
compute summarize:
  output: child_out
  expr:
    verdict: "outputs.run_child.verdict"

workflow gate_parent:
  entry: prep
  prep -> run_child
  run_child -> summarize
  summarize -> done
`

func TestServiceLaunch_SubbotChildHumanGate_ParkAndResume(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate_child.bot"), []byte(subbotGateChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "gate_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotGateParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Wait for the child run to appear paused on its human gate. The child
	// is discovered by ParentRunID — the same linkage the pipeline board's
	// tree folding uses.
	childID := ""
	deadline := time.Now().Add(30 * time.Second)
	for childID == "" {
		if time.Now().After(deadline) {
			t.Fatal("child run never reached paused_waiting_human")
		}
		runs, lerr := svc.ListRunRecordsCtx(context.Background(), ListFilter{})
		if lerr != nil {
			t.Fatalf("list runs: %v", lerr)
		}
		for _, r := range runs {
			if r.ParentRunID == res.RunID && r.Status == store.RunStatusPausedWaitingHuman {
				childID = r.ID
			}
		}
		if childID == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}

	// The parent must still be RUNNING (parked on the child), not failed.
	parent, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if parent.Status != store.RunStatusRunning {
		t.Fatalf("parent status while child paused = %q, want running", parent.Status)
	}

	// Answer the child's gate the way the pipeline-board sidebar does:
	// resume the CHILD run id with the answers map. The sidebar sends no
	// source, so the server falls back to the run's persisted FilePath —
	// mirrored here by passing the child's own .bot (assert it was
	// persisted by the in-process runner first).
	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.FilePath == "" {
		t.Fatal("child run has no persisted FilePath — the sidebar's resume (source=null) would 409")
	}
	if _, err := svc.Resume(context.Background(), ResumeSpec{
		RunID:    childID,
		FilePath: child.FilePath,
		Answers:  map[string]any{"approved": true, "notes": "ship it"},
	}); err != nil {
		t.Fatalf("resume child: %v", err)
	}

	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("parent did not finish after the child's gate was answered")
	}

	parent, err = svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if parent.Status != store.RunStatusFinished {
		t.Fatalf("parent status = %q (error %q), want finished", parent.Status, parent.Error)
	}

	// The parent's downstream compute consumed {{outputs.run_child.verdict}}
	// — the child's terminal output, derived from the human answer. Its
	// node_finished event carries the output payload; that's the durable
	// proof the output survived the external resume.
	verdict := ""
	if err := svc.store.ScanEvents(context.Background(), res.RunID, func(e *store.Event) bool {
		if e.Type == store.EventNodeFinished && e.NodeID == "summarize" {
			if out, ok := e.Data["output"].(map[string]any); ok {
				verdict, _ = out["verdict"].(string)
			}
		}
		return true
	}); err != nil {
		t.Fatalf("scan parent events: %v", err)
	}
	if verdict != "ship it" {
		t.Errorf("summarize verdict = %q, want %q (child terminal output lost across the external resume)", verdict, "ship it")
	}

	child, err = svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("reload child: %v", err)
	}
	if child.Status != store.RunStatusFinished {
		t.Errorf("child status = %q, want finished", child.Status)
	}
}
