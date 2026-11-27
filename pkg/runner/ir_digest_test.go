package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// irProgram marshals a workflow named name that goes straight to done — no
// node that could reach a model, whichever program a defect loads — the
// bytes a publisher offloads for one publication.
func irProgram(t *testing.T, name string) []byte {
	t.Helper()
	src := "workflow " + name + ":\n  entry: done\n"
	body, err := ast.MarshalFile(parsertest.Parse("main.bot", src).File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}
	return body
}

// offloadIR stores body as one publication of runID does — under its
// content id, through the IR blob seam — and returns the reference that
// publication's message carries.
func offloadIR(t *testing.T, blobs store.IRBlobStore, runID string, body []byte) *queue.IRRef {
	t.Helper()
	digest := queue.IRDigest(body)
	id, err := store.IRContentLogicalID(runID, digest, rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	key, err := blobs.PutIRBlob(context.Background(), id, body)
	if err != nil {
		t.Fatalf("PutIRBlob: %v", err)
	}
	return &queue.IRRef{StorageKey: key, Backend: queue.IRBackendS3, Digest: digest}
}

// A redelivery of an older message, after a later publication of the same
// run (a resume) was stored — and after an older publisher still serving
// mid-rollout rewrote the run's legacy ir/<run>.json — loads the program
// ITS message published, never the later one.
func TestLoadWorkflow_RedeliveryAfterALaterPublicationRunsItsOwnProgram(t *testing.T) {
	blobs := &fakeIRBlobs{}
	first := offloadIR(t, blobs, "run-big", irProgram(t, "first"))
	offloadIR(t, blobs, "run-big", irProgram(t, "later"))
	if _, err := blobs.PutIRBlob(context.Background(), "run-big", irProgram(t, "later")); err != nil {
		t.Fatal(err)
	}
	msg := &queue.RunMessage{RunID: "run-big", WorkflowName: "first", IRRef: first}
	wf, err := loadWorkflow(context.Background(), msg, blobs, nil)
	if err != nil {
		t.Fatalf("the redelivered message's own program does not load: %v", err)
	}
	if wf.Name != "first" {
		t.Fatalf("the redelivered message ran workflow %q, want its own %q", wf.Name, "first")
	}
}

// Bytes under the message's key that do not hash to its digest are not the
// program that message published: refused as ErrIRDigestMismatch — an
// ErrIRUnloadable, so the delivery is acked — with both digests named.
func TestLoadWorkflow_RefusesAnIRThatMissesItsDigest(t *testing.T) {
	blobs := &fakeIRBlobs{}
	ref := offloadIR(t, blobs, "run-big", irProgram(t, "first"))
	later := irProgram(t, "later")
	blobs.blobs[ref.StorageKey] = later
	msg := &queue.RunMessage{RunID: "run-big", WorkflowName: "first", IRRef: ref}
	wf, err := loadWorkflow(context.Background(), msg, blobs, nil)
	if !errors.Is(err, ErrIRDigestMismatch) || !errors.Is(err, ErrIRUnloadable) {
		t.Fatalf("loadWorkflow = (%v, %v), want ErrIRDigestMismatch (an ErrIRUnloadable)", wf, err)
	}
	for _, want := range []string{ref.StorageKey, ref.Digest, queue.IRDigest(later)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %s", err, want)
		}
	}
}

// A legacy reference — ir/<run>.json, no digest, what a publisher of the
// previous release still sends mid-rollout — loads, and the runner logs
// that it loaded it without the digest check.
func TestLoadWorkflow_LegacyIRRefLoadsAndSaysItWasUnchecked(t *testing.T) {
	blobs := &fakeIRBlobs{}
	key, err := blobs.PutIRBlob(context.Background(), "run-big", irProgram(t, "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	var sink syncBuffer
	logger := iterlog.New(iterlog.LevelDebug, &sink)
	msg := &queue.RunMessage{RunID: "run-big", WorkflowName: "legacy", IRRef: &queue.IRRef{StorageKey: key, Backend: queue.IRBackendS3}}
	wf, err := loadWorkflow(context.Background(), msg, blobs, logger)
	if err != nil {
		t.Fatalf("legacy reference refused: %v", err)
	}
	if wf.Name != "legacy" {
		t.Fatalf("workflow = %q, want legacy", wf.Name)
	}
	if out := sink.String(); !strings.Contains(out, "carries no digest") || !strings.Contains(out, key) {
		t.Fatalf("no log line says the legacy IR %s loaded unchecked: %q", key, out)
	}
}

// irBlobFSStore is a filesystem run store that also serves IR blobs, so
// executeRun reaches loadWorkflow's fetch and the refusal lands on a real
// run document.
type irBlobFSStore struct {
	*store.FilesystemRunStore
	*fakeIRBlobs
}

// The wiring: executeRun, handed a message whose stored IR misses its
// digest, writes the resumable verdict on the run with the way out that
// fits — resume to republish — not the fleet-skew advice, and the delivery
// is acked.
func TestExecuteRun_DigestMismatchFailsResumableWithTheRepublishHint(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fs.CreateRun(ctx, "run-tampered", "first", nil); err != nil {
		t.Fatal(err)
	}
	blobs := &fakeIRBlobs{}
	ref := offloadIR(t, blobs, "run-tampered", irProgram(t, "first"))
	blobs.blobs[ref.StorageKey] = irProgram(t, "later")
	r := &Runner{cfg: Config{Store: &irBlobFSStore{FilesystemRunStore: fs, fakeIRBlobs: blobs}, Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	msg := &queue.RunMessage{RunID: "run-tampered", WorkflowName: "first", WorkflowHash: "e7e2f6e6", IRRef: ref}
	execErr := r.executeRun(ctx, msg, nil, nil)
	if !errors.Is(execErr, ErrIRDigestMismatch) {
		t.Fatalf("executeRun err = %v, want ErrIRDigestMismatch", execErr)
	}
	if got := classifyExecResult(execErr, msg.RunID); got.action != actionAck || got.finalStatus != "ir_unloadable" {
		t.Fatalf("classified as %s/%v, want ir_unloadable/ack", got.finalStatus, got.action)
	}
	run, err := fs.LoadRun(ctx, "run-tampered")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFailedResumable || run.FailureCode != store.FailureIRUnloadable {
		t.Fatalf("run = %s/%s, want failed_resumable/IR_UNLOADABLE", run.Status, run.FailureCode)
	}
	if run.PreEngineRefusal == nil || !strings.Contains(run.PreEngineRefusal.Hint, "republish") || strings.Contains(run.PreEngineRefusal.Hint, "align the runner") {
		t.Fatalf("refusal = %+v, want the republish hint, not the fleet-skew one", run.PreEngineRefusal)
	}
}

// The delivery log of a digest mismatch names its cause and its way out —
// a resume republishes the program — not the fleet-skew advice of an IR the
// runner cannot compile.
func TestClassifyExecResult_DigestMismatchNamesItsWayOut(t *testing.T) {
	got := classifyExecResult(errors.Join(errors.New("runner"), ErrIRDigestMismatch), "run-x")
	if got.action != actionAck || got.finalStatus != "ir_unloadable" {
		t.Fatalf("classified as %s/%v, want ir_unloadable/ack", got.finalStatus, got.action)
	}
	if !strings.Contains(got.logFmt, "republish") || strings.Contains(got.logFmt, "align the runner") {
		t.Fatalf("log %q gives the fleet-skew advice for a digest mismatch", got.logFmt)
	}
}
