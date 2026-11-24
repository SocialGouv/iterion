package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Round 2 of the adversarial loop on #2086, the runtime side.

// The uploads are promoted BEFORE the engine on a hand-off path (the cloud
// publisher, a detached runner); the engine's bundle defaults then run on a
// run that already holds the operator's file, and must not replace it.
func TestABundleDefaultNeverReplacesAnOperatorUpload(t *testing.T) {
	ctx := context.Background()
	rs := tmpStore(t)
	const runID = "run_default_vs_upload"
	if _, err := rs.CreateRun(ctx, runID, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteAttachment(ctx, runID, store.AttachmentRecord{Name: "brief", OriginalFilename: "mine.md", MIME: "text/markdown"}, strings.NewReader("USER UPLOAD")); err != nil {
		t.Fatal(err)
	}
	bundleDir := t.TempDir()
	attachmentsDir := filepath.Join(bundleDir, "attachments")
	if err := os.MkdirAll(attachmentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachmentsDir, "default-brief.md"), []byte("BUNDLE DEFAULT"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &bundle.Bundle{Dir: bundleDir, AttachmentsDir: attachmentsDir, Manifest: &bundle.Manifest{
		Attachments: map[string]string{"brief": "default-brief.md", "notes": "default-brief.md"},
	}}
	wf := &ir.Workflow{Attachments: map[string]*ir.Attachment{"brief": {Name: "brief"}, "notes": {Name: "notes"}}}
	if err := promoteBundleAttachmentDefaults(ctx, rs, runID, wf, b, nil); err != nil {
		t.Fatalf("promoteBundleAttachmentDefaults: %v", err)
	}
	read := func(name string) (string, store.AttachmentRecord) {
		rc, rec, err := rs.OpenAttachment(ctx, runID, name)
		if err != nil {
			t.Fatalf("OpenAttachment(%q): %v", name, err)
		}
		defer rc.Close()
		data, _ := io.ReadAll(rc)
		return string(data), rec
	}
	if body, rec := read("brief"); body != "USER UPLOAD" || rec.OriginalFilename != "mine.md" {
		t.Fatalf("brief = %q (file %q), want the operator's upload", body, rec.OriginalFilename)
	}
	if body, _ := read("notes"); body != "BUNDLE DEFAULT" {
		t.Fatalf("notes = %q, want the default nobody replaced", body)
	}
}

// A fetch cut short by the run's own context is the cancel's (or the
// drain's): a cancel the cloud already wrote is never rewritten into a
// resumable failure that a redelivery would resume.
func TestACancelDuringTheFetchStaysTheOperatorsCancel(t *testing.T) {
	cause := attachmentsUnfetchable("run-x", "fetch attachment spec", context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if status, _, code := setupFailureStatus(ctx, "sandbox start", cause); status != store.RunStatusCancelled || code != store.FailureCancelled {
		t.Fatalf("cancelled ctx: %s / %s, want cancelled / %s", status, code, store.FailureCancelled)
	}
	ictx, icancel := context.WithCancelCause(context.Background())
	icancel(ErrRunInterrupted)
	if status, _, code := setupFailureStatus(ictx, "sandbox start", cause); status != store.RunStatusFailedResumable || code != store.FailureInterrupted {
		t.Fatalf("drained ctx: %s / %s, want failed_resumable / %s", status, code, store.FailureInterrupted)
	}
}

// Only the image references the claw executor turns into image blocks are
// exempt from the path: the USER prompt's, when every route is claw.
func TestTheImageBlockScopeIsTheClawUserPromptAlone(t *testing.T) {
	w := &ir.Workflow{
		Attachments: map[string]*ir.Attachment{"logo": {Name: "logo", Type: ir.AttachmentImage}},
		Prompts: map[string]*ir.Prompt{
			"see":   {Name: "see", Body: "look {{attachments.logo}}", TemplateRefs: refsOf(t, "look {{attachments.logo}}")},
			"brand": {Name: "brand", Body: "respect {{attachments.logo}}", TemplateRefs: refsOf(t, "respect {{attachments.logo}}")},
		},
	}
	td := &model.TemplateData{Attachments: map[string]model.AttachmentInfo{"logo": {Name: "logo"}}}
	claw := func(system, user string, fallbacks ...ir.Fallback) *ir.AgentNode {
		n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "ask"}}
		n.Backend = "claw"
		n.SystemPrompt = system
		n.UserPrompt = user
		n.Fallbacks = fallbacks
		return n
	}
	if err := admitAttachmentPaths(claw("", "see"), w, td, nil); err != nil {
		t.Fatalf("a claw user prompt's image reference was refused: %v", err)
	}
	if err := admitAttachmentPaths(claw("", "see", ir.Fallback{Name: "same"}), w, td, nil); err != nil {
		t.Fatalf("a fallback on the node's own backend was refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		node *ir.AgentNode
	}{
		{"a claw system prompt interpolates the path", claw("brand", "see")},
		{"a fallback on a CLI backend interpolates the path", claw("", "see", ir.Fallback{Name: "cli", Backend: "claude_code"})},
		{"a user prompt that is also the system prompt", claw("see", "see")},
	} {
		if err := admitAttachmentPaths(tc.node, w, td, nil); err == nil {
			t.Fatalf("%s: admitted with no path", tc.name)
		}
	}
}

// ghostListStore answers a record no name reaches beside the run's real
// ones: the empty entry a dotted name left in a map written through a path.
type ghostListStore struct{ store.RunStore }

func (ghostListStore) Root() string { return "" }

func (g ghostListStore) ListAttachments(ctx context.Context, runID string) ([]store.AttachmentRecord, error) {
	list, err := g.RunStore.ListAttachments(ctx, runID)
	return append(list, store.AttachmentRecord{}), err
}

// Such a record can be neither referenced nor copied, and a redelivery
// meets it again: it is skipped, never a fetch blip that burns deliveries.
func TestARecordNoNameReachesIsSkippedNotRetried(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fs := tmpStore(t)
	attachedRun(t, fs, "run-ghost")
	e := engineOn(t, ghostListStore{fs})
	dir, err := e.materializeAttachments(context.Background(), "run-ghost")
	if err != nil {
		var rt *RuntimeError
		errors.As(err, &rt)
		t.Fatalf("a record no name reaches failed the materialisation: %v (code %v)", err, rt)
	}
	if _, err := os.Stat(filepath.Join(dir, "spec", "spec.md")); err != nil {
		t.Fatalf("the run's real attachment was not materialised: %v", err)
	}
	if _, err := fs.CreateRun(context.Background(), "run-only-ghost", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if dir, err := e.materializeAttachments(context.Background(), "run-only-ghost"); err != nil || dir != "" {
		t.Fatalf("a run holding nothing but a ghost = %q, %v; want no directory and no error", dir, err)
	}
}
