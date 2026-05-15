package runtime

import (
	"context"
	"os"
	"path/filepath"
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
