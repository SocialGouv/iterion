package server

import (
	"context"
	"strings"
	"testing"
)

// `meta.json` is an ordinary filename since the record sidecar moved out
// of the bytes' namespace (#2863): the door admits it, and the promoted
// attachment reads back with its bytes and its record intact.
func TestUploadAttachment_AdmitsTheFormerSidecarName(t *testing.T) {
	srv := uploadTestServer(t)
	id := stageOne(t, srv, "meta.json", "application/json", []byte(`{"user":"payload"}`))
	if _, err := srv.runs.RunStore().CreateRun(context.Background(), "run-meta-001", "demo", nil); err != nil {
		t.Fatalf("create the run: %v", err)
	}
	if _, _, err := srv.promoteStaged(context.Background(), "run-meta-001", map[string]string{"spec": id}); err != nil {
		t.Fatalf("promote the meta.json upload: %v", err)
	}
	rc, rec, err := srv.runs.OpenAttachment(context.Background(), "run-meta-001", "spec")
	if err != nil {
		t.Fatalf("open the promoted attachment: %v", err)
	}
	defer rc.Close()
	data := make([]byte, 64)
	n, _ := rc.Read(data)
	if string(data[:n]) != `{"user":"payload"}` || rec.OriginalFilename != "meta.json" {
		t.Fatalf("read back %q / %q, want the payload under meta.json", data[:n], rec.OriginalFilename)
	}
}

// An attachment name becomes a directory entry a driver hands to tar by
// name; one that reads as an option is refused before anything moves.
func TestPromoteStagedRefusesAnOptionLikeName(t *testing.T) {
	srv := uploadTestServer(t)
	id := stageOne(t, srv, "spec.md", "text/markdown", []byte("# spec"))
	_, _, err := srv.promoteStaged(context.Background(), "run-opt-001", map[string]string{"--checkpoint=1": id})
	if err == nil || !strings.Contains(err.Error(), "must not start with a dash") {
		t.Fatalf("err = %v, want the dash refusal", err)
	}
}
