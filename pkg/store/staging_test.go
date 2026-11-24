package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// "." passed the component rule and joined to the staging ROOT: discarding
// that id discarded every upload. The rule refuses it now, at the one
// sanitiser every store path crosses.
func TestDiscardingTheStagingRootIsRefused(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"up_1700000000_000000000000000a", "up_1700000000_000000000000000b"} {
		if err := s.StageUpload(ctx, StagedUpload{UploadID: id, OriginalFilename: "x.txt", MIME: "text/plain", Size: 1, SHA256: "x"}, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("stage %s: %v", id, err)
		}
	}
	if err := s.DiscardStagedUpload(ctx, "."); err == nil {
		t.Fatal("discarding \".\" was accepted")
	}
	for _, id := range []string{"up_1700000000_000000000000000a", "up_1700000000_000000000000000b"} {
		if _, rc, err := s.OpenStagedUpload(ctx, id); err != nil {
			t.Fatalf("%s is gone: %v", id, err)
		} else {
			_ = rc.Close()
		}
	}
	if _, _, err := s.OpenStagedUpload(ctx, "."); err == nil || errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("opening \".\" = %v, want the sanitiser's refusal", err)
	}
}
