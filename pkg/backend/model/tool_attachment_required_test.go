package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runDeclaringNode runs ONE direct tool node whose stdout carries the
// directive under test, through the public path an author reaches:
// ClawExecutor.Execute, the real store-event hooks, and a real
// FilesystemRunStore as the attachment sink. Nothing is stubbed — the
// hooks layer is where the delivery lives, so a double there would
// prove the test's own wiring instead of the engine's.
//
// It returns every event the run recorded and the node's error.
func runDeclaringNode(t *testing.T, body string) ([]*store.Event, error) {
	t.Helper()
	return runDeclaringToolNode(t, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "final_report"},
		Language: "sh",
		Script:   body,
	})
}

// runDeclaringToolNode is the same, for a node the caller shapes itself
// (a postcondition sends it through the Verified Action ladder, which
// reaches the delivery from a different rung).
func runDeclaringToolNode(t *testing.T, node *ir.ToolNode) ([]*store.Event, error) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-deliverable"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	// A REAL guard, not nil: every event the hooks append goes through
	// redaction in production, and a `required` field the redactor
	// dropped would read as "not required" to every consumer. A nil
	// guard would prove the kinder path.
	guard := secretguard.New(
		[]secretguard.Secret{{Name: "api_key", Value: "sk-ant-NOT-IN-THIS-TEST-0123456789"}},
		secretguard.DefaultConfig(),
	)
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolPolicy(tool.OpenPolicy()),
		WithEventHooks(NewStoreEventHooks(ctx, st, runID, logger, guard, nil)),
	)
	_, runErr := exec.Execute(ctx, node, map[string]any{})

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	return evts, runErr
}

// lossEvents returns the run_attachment_lost events, in the order the
// run recorded them.
func lossEvents(evts []*store.Event) []*store.Event {
	var found []*store.Event
	for _, evt := range evts {
		if evt.Type == store.EventRunAttachmentLost {
			found = append(found, evt)
		}
	}
	return found
}

// TestARequiredDeliverableLostFailsTheNode is the witness for #2668's
// opt-in half: `required=true` turns a declared file the run never
// received into the NODE's failure, while the same loss without the
// token stays exactly what it was — an event beside a successful node.
//
// Both halves run through the same body and the same executor, so the
// token is the only difference between a green node and a red one.
func TestARequiredDeliverableLostFailsTheNode(t *testing.T) {
	t.Parallel()
	t.Run("required: the node fails and names the file", func(t *testing.T) {
		// The pod case in miniature: the path the tool declares cannot be
		// read from here, so the bytes never reach the run.
		absent := filepath.Join(t.TempDir(), "report.md")
		evts, err := runDeclaringNode(t,
			"echo '[iterion] attachment="+absent+" required=true'")

		if err == nil {
			t.Fatalf("the node succeeded while the file it declared "+
				"required=true never reached the run (%s)", absent)
		}
		for _, want := range []string{"final_report", absent, "required", "unreadable"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("node error %q lacks %q — the operator must learn "+
					"WHICH declared file failed the node", err, want)
			}
		}
		// The failure does not replace the record: the loss is still on
		// the run, and it says it was required.
		losses := lossEvents(evts)
		if len(losses) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d: %#v",
				store.EventRunAttachmentLost, len(losses), evts)
		}
		if got := losses[0].Data["required"]; got != true {
			t.Errorf("event required = %#v, want true — a reader filtering "+
				"the losses that failed a node has only this field", got)
		}
		if got := losses[0].Data["path"]; got != absent {
			t.Errorf("event path = %v, want %s", got, absent)
		}
	})

	t.Run("not required: the event alone, the node keeps its result", func(t *testing.T) {
		absent := filepath.Join(t.TempDir(), "report.md")
		evts, err := runDeclaringNode(t,
			"echo '[iterion] attachment="+absent+"'")

		if err != nil {
			t.Fatalf("a declared file the run did not receive failed a node "+
				"that never asked for the guarantee: %v", err)
		}
		losses := lossEvents(evts)
		if len(losses) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d: %#v",
				store.EventRunAttachmentLost, len(losses), evts)
		}
		if got := losses[0].Data["required"]; got != false {
			t.Errorf("event required = %#v, want false", got)
		}
	})

	t.Run("required and delivered: nothing fails", func(t *testing.T) {
		// The guarantee is about the LOSS, not about the token: a
		// required file that arrives must leave the node green, or
		// every bot that opts in breaks on its happy path.
		dir := t.TempDir()
		path := filepath.Join(dir, "report.md")
		if err := os.WriteFile(path, []byte("# audit\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		evts, err := runDeclaringNode(t,
			"echo '[iterion] attachment="+path+" required=true'")

		if err != nil {
			t.Fatalf("a required deliverable that REACHED the run failed the node: %v", err)
		}
		if losses := lossEvents(evts); len(losses) != 0 {
			t.Fatalf("a published file recorded a loss: %#v", losses)
		}
		published := false
		for _, evt := range evts {
			if evt.Type == store.EventRunAttachmentPublished {
				published = true
			}
		}
		if !published {
			t.Error("no run_attachment_published event for a file that was delivered")
		}
	})

	t.Run("a failing recipe is judged by its own error", func(t *testing.T) {
		// A recipe that died mid-write owes nothing extra: its exit
		// status is the story, and a second verdict about the file it
		// never finished would bury it.
		absent := filepath.Join(t.TempDir(), "report.md")
		evts, err := runDeclaringNode(t,
			"echo '[iterion] attachment="+absent+" required=true'; echo BOOM 1>&2; exit 3")

		if err == nil {
			t.Fatal("a node that exited 3 succeeded")
		}
		if !strings.Contains(err.Error(), "shell command failed") &&
			!strings.Contains(err.Error(), "script failed") {
			t.Errorf("node error = %q, want the recipe's own failure", err)
		}
		if strings.Contains(err.Error(), "was not delivered") {
			t.Errorf("node error = %q — the delivery verdict replaced the "+
				"recipe's own exit status", err)
		}
		if losses := lossEvents(evts); len(losses) != 0 {
			t.Errorf("a failed recipe recorded a delivery loss: %#v", losses)
		}
	})
}

// TestARequiredDeliverableLostFailsAVerifiedNode is the second public
// path: a node with a postcondition runs the Verified Action ladder,
// whose oracle is the postcondition and not the recipe's exit code. A
// met postcondition would hand that node a success — so the lost
// deliverable has to end it there too, or the guarantee holds on one
// kind of tool node and not the other.
func TestARequiredDeliverableLostFailsAVerifiedNode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	absent := filepath.Join(dir, "report.md")

	// The postcondition is unmet at rung 1 (so the recipe runs) and met
	// after it (so the ladder would call the node a success).
	evts, err := runDeclaringToolNode(t, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "final_report"},
		Language: "sh",
		Script: "touch " + marker +
			"; echo '[iterion] attachment=" + absent + " required=true'",
		Postcondition: "test -f " + marker,
	})

	if err == nil {
		t.Fatalf("the ladder called the node a success on a met postcondition "+
			"while the file it declared required=true never reached the run (%s)", absent)
	}
	if !strings.Contains(err.Error(), "was not delivered") {
		t.Errorf("node error = %q, want the delivery verdict", err)
	}
	if strings.Contains(err.Error(), "postcondition not met") {
		t.Errorf("node error = %q — the postcondition WAS met; the loss is "+
			"what failed the node", err)
	}
	if losses := lossEvents(evts); len(losses) != 1 {
		t.Errorf("want exactly 1 %s event, got %d",
			store.EventRunAttachmentLost, len(losses))
	}
}

// TestTheRequiredTokenIsParsedFromTheDirective pins the grammar half:
// the token is recognised beside the other options, after a path that
// contains spaces, and an explicit `false` is honoured as the opt-out
// it reads as.
func TestTheRequiredTokenIsParsedFromTheDirective(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want AttachmentDirective
	}{
		{
			"beside name and mime",
			"[iterion] attachment=/exports/report.md name=audit mime=text/markdown required=true",
			AttachmentDirective{
				Path: "/exports/report.md", Name: "audit",
				MIME: "text/markdown", Required: true,
			},
		},
		{
			"after a path with spaces",
			"[iterion] attachment=/exports/final cut.mp4 required=true",
			AttachmentDirective{Path: "/exports/final cut.mp4", Required: true},
		},
		{
			"explicit false is the opt-out",
			"[iterion] attachment=/exports/report.md required=false",
			AttachmentDirective{Path: "/exports/report.md"},
		},
		{
			"absent is the default",
			"[iterion] attachment=/exports/report.md",
			AttachmentDirective{Path: "/exports/report.md"},
		},
		{
			// Fail-closed: the token is only ever typed to ASK for the
			// guarantee, so a spelling no parser knows must not cancel it.
			"an unparseable value still requires",
			"[iterion] attachment=/exports/report.md required=yes",
			AttachmentDirective{Path: "/exports/report.md", Required: true},
		},
		{
			"an empty value still requires",
			"[iterion] attachment=/exports/report.md required=",
			AttachmentDirective{Path: "/exports/report.md", Required: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanToolAttachments(tc.line)
			if len(got) != 1 {
				t.Fatalf("scanToolAttachments = %#v, want 1 directive", got)
			}
			if got[0] != tc.want {
				t.Errorf("directive = %#v, want %#v", got[0], tc.want)
			}
		})
	}
}
