package runner

import (
	"errors"
	"fmt"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The store did not hand the run's attachments over before the sandbox
// (ATTACHMENTS_UNFETCHABLE, failed_resumable): re-offered after a delay
// like its two resumable siblings — an immediate Nak spends every
// permitted delivery on the same storage blip, then the DLQ park writes
// DLQ_PARKED over the code.
func TestClassifyExecResult_AnUnfetchableAttachmentIsReofferedAfterADelay(t *testing.T) {
	err := fmt.Errorf("runtime: sandbox: %w", &runtime.RuntimeError{
		Code:    runtime.ErrCodeAttachmentsUnfetchable,
		Message: "the run's attachments could not be materialised for the sandbox of run run-1 (fetch attachment spec)",
		Cause:   errors.New("the bucket is unreachable"),
	})
	out := classifyExecResult(err, "run-1")
	if out.finalStatus != "attachments_unfetchable" {
		t.Fatalf("finalStatus = %q, want attachments_unfetchable", out.finalStatus)
	}
	if out.delay != attachmentsFetchNakDelay || out.delay <= 0 {
		t.Fatalf("delay = %s, want %s — an immediate re-offer spends every delivery on one blip", out.delay, attachmentsFetchNakDelay)
	}
	if bankableStatus(out.finalStatus) {
		t.Fatal("a run that executed nothing has nothing to bank")
	}
	d := &fakeDelivery{delivered: 3}
	dispatchExecOutcome(iterlog.Nop(), d, out, "run-1")
	if len(d.nakDelays) != 1 || d.nakDelays[0] != attachmentsFetchNakDelay || d.naks != 0 || d.terms != 0 || d.acks != 0 {
		t.Fatalf("delivery transitions = %+v, want exactly one NakWithDelay(%s)", d, attachmentsFetchNakDelay)
	}
}
