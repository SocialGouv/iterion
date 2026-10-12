package blob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// The scratch bank (ADR-106) is one object beside the run's packed CLI
// sessions: streamed in and out, absent → ErrArtifactNotFound, and swept with
// the run's sessions/ prefix.
func TestS3Client_ScratchBankRoundTripLayoutAndSweep(t *testing.T) {
	fake, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	ctx := context.Background()

	if _, err := c.OpenScratchBank(ctx, "run-001"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("OpenScratchBank without a bank: %v, want ErrArtifactNotFound", err)
	}
	body := "gzip'd tar of the scratch"
	if err := c.PutScratchBank(ctx, "run-001", strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutScratchBank: %v", err)
	}
	if got := strings.Join(fake.keys(), " "); got != "sessions/run-001/scratch.tgz" {
		t.Fatalf("object keys %q, want the bank beside the run's sessions", got)
	}
	rc, err := c.OpenScratchBank(ctx, "run-001")
	if err != nil {
		t.Fatalf("OpenScratchBank: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != body {
		t.Fatalf("OpenScratchBank = %q, %v; want %q", got, err, body)
	}
	if err := c.DeleteRunBackendSessions(ctx, "run-001"); err != nil {
		t.Fatalf("DeleteRunBackendSessions: %v", err)
	}
	if keys := fake.keys(); len(keys) != 0 {
		t.Fatalf("the run's sessions sweep left %v", keys)
	}
	if err := c.DeleteScratchBank(ctx, "run-001"); err != nil {
		t.Fatalf("DeleteScratchBank of an absent bank: %v", err)
	}
}
