package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPromoteBundleAttachmentDefaults_WritesDeclaredAttachments(t *testing.T) {
	storeDir := t.TempDir()
	rs, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := "run_test"
	if _, err := rs.CreateRun(ctx, runID, "", nil); err != nil {
		t.Fatal(err)
	}

	bundleDir := t.TempDir()
	attachmentsDir := filepath.Join(bundleDir, "attachments")
	if err := os.MkdirAll(attachmentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachmentsDir, "logo.png"), []byte("\x89PNG\r\n\x1a\n"+"fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &bundle.Bundle{
		Dir:            bundleDir,
		AttachmentsDir: attachmentsDir,
		Manifest: &bundle.Manifest{
			Attachments: map[string]string{
				"logo": "logo.png",
			},
		},
	}
	wf := &ir.Workflow{
		Attachments: map[string]*ir.Attachment{
			"logo": {Name: "logo", Type: ir.AttachmentImage},
		},
	}

	if err := promoteBundleAttachmentDefaults(ctx, rs, runID, wf, b, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	r, err := rs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := r.Attachments["logo"]
	if !ok {
		t.Fatalf("logo not promoted: %+v", r.Attachments)
	}
	if rec.OriginalFilename != "logo.png" {
		t.Errorf("OriginalFilename = %q", rec.OriginalFilename)
	}
	if rec.MIME == "" {
		t.Errorf("MIME empty")
	}
}

func TestPromoteBundleAttachmentDefaults_SkipsAttachmentsNotDeclaredInWorkflow(t *testing.T) {
	storeDir := t.TempDir()
	rs, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := "run_test"
	if _, err := rs.CreateRun(ctx, runID, "", nil); err != nil {
		t.Fatal(err)
	}

	bundleDir := t.TempDir()
	attachmentsDir := filepath.Join(bundleDir, "attachments")
	if err := os.MkdirAll(attachmentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachmentsDir, "ghost.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &bundle.Bundle{
		Dir:            bundleDir,
		AttachmentsDir: attachmentsDir,
		Manifest:       &bundle.Manifest{Attachments: map[string]string{"ghost": "ghost.txt"}},
	}
	wf := &ir.Workflow{Attachments: map[string]*ir.Attachment{}} // empty

	if err := promoteBundleAttachmentDefaults(ctx, rs, runID, wf, b, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	r, _ := rs.LoadRun(ctx, runID)
	if rec, ok := r.Attachments["ghost"]; ok {
		t.Errorf("ghost should not be promoted (not in workflow), got %+v", rec)
	}
}

func TestPromoteBundleAttachmentDefaults_NilBundleIsNoop(t *testing.T) {
	storeDir := t.TempDir()
	rs, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := promoteBundleAttachmentDefaults(context.Background(), rs, "run", nil, nil, nil); err != nil {
		t.Errorf("nil bundle should be a no-op, got %v", err)
	}
}

// A bundle whose manifest declares defaults the workflow references, with no
// `attachments/` directory to read them from, must fail the run instead of
// starting it without them. The same fault one file at a time — a declared
// path missing from an existing directory — already fails (os.Open); folding
// the whole-directory case into the no-op guard dropped EVERY default of that
// bot silently, in every run including scheduled ones, which is the operator
// complaint behind #1908.
func TestPromoteBundleAttachmentDefaults_RefusesDeclaredDefaultsWithNoAttachmentsDir(t *testing.T) {
	storeDir := t.TempDir()
	rs, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := "run_test"
	if _, err := rs.CreateRun(ctx, runID, "", nil); err != nil {
		t.Fatal(err)
	}

	// Two names, declared in an order the sort has to undo, so the refusal
	// is proven deterministic rather than whatever the map range yields.
	b := &bundle.Bundle{
		Dir: t.TempDir(),
		Manifest: &bundle.Manifest{Attachments: map[string]string{
			"zz-policy": "policy.pdf",
			"aa-guide":  "guide.pdf",
		}},
		// AttachmentsDir deliberately empty: the loader leaves it so when
		// <bundle>/attachments is not on disk (pkg/bundle/loader.go).
	}
	wf := &ir.Workflow{Attachments: map[string]*ir.Attachment{
		"aa-guide":  {Name: "aa-guide", Type: ir.AttachmentFile},
		"zz-policy": {Name: "zz-policy", Type: ir.AttachmentFile},
	}}

	err = promoteBundleAttachmentDefaults(ctx, rs, runID, wf, b, nil)
	if err == nil {
		t.Fatal("a bundle declaring attachment defaults it cannot deliver promoted nothing and reported nothing")
	}
	if got := err.Error(); !strings.Contains(got, `["aa-guide" "zz-policy"]`) {
		t.Errorf("the refusal does not name both undeliverable defaults in a stable order: %q", got)
	}
	if got := err.Error(); !strings.Contains(got, bundle.DirAttachments+"/") {
		t.Errorf("the refusal does not name the missing directory: %q", got)
	}
	r, err := rs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Attachments) != 0 {
		t.Errorf("a refused promotion still wrote attachments: %+v", r.Attachments)
	}
}

// The other direction, and the reason the refusal sits AFTER the
// declaration filter: a manifest default no workflow name references was
// already a no-op (warned and skipped), with or without the directory.
// Refusing it would fail runs that behave identically today.
func TestPromoteBundleAttachmentDefaults_NoAttachmentsDirIsANoopWhenTheWorkflowReferencesNothing(t *testing.T) {
	storeDir := t.TempDir()
	rs, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := "run_test"
	if _, err := rs.CreateRun(ctx, runID, "", nil); err != nil {
		t.Fatal(err)
	}

	b := &bundle.Bundle{
		Dir:      t.TempDir(),
		Manifest: &bundle.Manifest{Attachments: map[string]string{"ghost": "ghost.txt"}},
	}
	wf := &ir.Workflow{Attachments: map[string]*ir.Attachment{}} // references none

	if err := promoteBundleAttachmentDefaults(ctx, rs, runID, wf, b, nil); err != nil {
		t.Fatalf("a default the workflow does not reference must stay a skip, got %v", err)
	}
	r, _ := rs.LoadRun(ctx, runID)
	if len(r.Attachments) != 0 {
		t.Errorf("nothing should have been promoted: %+v", r.Attachments)
	}
}
