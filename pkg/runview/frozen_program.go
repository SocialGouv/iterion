package runview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// Frozen-program re-admission (ADR-167 T4a). A started run parked on a wait
// the retry sweeper ends — a capacity place lost while the queue redelivered
// it (CRED_SLOT_WAIT), a provider usage window — is re-admitted later by the
// sweeper. A redelivery would have run the program its message carried,
// whatever the bot became since; the re-admission keeps that contract where
// the park asks for it: the parking runner freezes the delivered program
// (FreezeProgram) into a content-addressed envelope the park's record names
// (store.FrozenProgram), and the re-admission republishes the envelope's IR
// and bundle reference verbatim (ResumeSpec.Frozen), never forced, its
// credentials and capacity resolved again as every resume's are. A capacity
// park's program is always re-admitted; a usage-window park's only when the
// bot's current program is refused because its source changed.
//
// The envelope holds the program and nothing else: no capacity holds, no
// sealed secrets or their key, no resume patch — those belong to an attempt,
// and the re-admission makes its own.

// frozenEnvelopeVersion is the envelope format FreezeProgram writes and
// LoadFrozenProgram reads.
const frozenEnvelopeVersion = 1

// FrozenEnvelope is the body of a frozen program's blob.
type FrozenEnvelope struct {
	V            int    `json:"v"`
	RunID        string `json:"run_id"`
	WorkflowName string `json:"workflow_name"`
	// WorkflowHash is the hash IR derives (TransportWorkflowHash).
	WorkflowHash string `json:"workflow_hash"`
	// IR is the transport IR the parked delivery carried, byte for byte: a
	// []byte (base64 on the wire) because a json.RawMessage would be
	// compacted on the way through.
	IR []byte `json:"ir"`
	// BotBundle is the delivered bundle reference with its snapshot stored
	// out of band (SnapshotRef, content-addressed), nil for an inline-source
	// run.
	BotBundle *queue.BotBundleRef `json:"bot_bundle,omitempty"`
}

// FrozenResume is what a re-admission of a frozen program carries: the run's
// record and the envelope it names, read and verified (LoadFrozenProgram).
type FrozenResume struct {
	Program  store.FrozenProgram
	Envelope *FrozenEnvelope
}

var (
	// ErrFreezeUnsupported: the delivered program cannot be frozen — it
	// carries no source snapshot to derive its hash from, or a legacy bundle
	// reference with no snapshot digest, or the store keeps no IR blobs.
	ErrFreezeUnsupported = errors.New("the delivered program cannot be frozen")
	// ErrFreezeHashMismatch: the delivered program does not derive the run's
	// workflow hash — the IR its message names was overwritten by a later
	// publication, or the run predates the source digest.
	ErrFreezeHashMismatch = errors.New("the delivered program does not derive the run's workflow hash")
	// ErrFrozenProgramUnavailable: the envelope a record names is missing,
	// does not hash to the record's digest, or does not describe the run.
	ErrFrozenProgramUnavailable = errors.New("the frozen program is unavailable")
	// ErrFrozenProgramStale: the record no longer binds the run (another
	// attempt, another program) at the moment the re-admission reads it.
	ErrFrozenProgramStale = errors.New("the frozen program no longer binds the run")
	// ErrFrozenResumeNotCloud: a frozen re-admission is a cloud publication;
	// a run service with no publisher has nothing to republish it through.
	ErrFrozenResumeNotCloud = errors.New("a frozen-program resume is a cloud publication only")
	// ErrCurrentProgramUnloadable: a resume of the bot's current program was
	// refused before that program's hash was known, because it does not load
	// or compile here (Service.Resume) — refused before anything moved or was
	// published. The retry sweeper reads it, beside a changed source, as the
	// bot having changed during a usage-window wait.
	ErrCurrentProgramUnloadable = errors.New("the current program does not load")
)

// currentProgramUnloadable types a resume's refusal of its current program
// as ErrCurrentProgramUnloadable, its text and its chain unchanged: every
// other caller reads the error it read before.
type currentProgramUnloadable struct{ err error }

func (e currentProgramUnloadable) Error() string { return e.err.Error() }

func (e currentProgramUnloadable) Unwrap() error { return e.err }

func (e currentProgramUnloadable) Is(target error) bool { return target == ErrCurrentProgramUnloadable }

// TransportIRUnloadable is DecodeTransportWorkflow's refusal of an IR that
// does not decode or compile here; Detail names the stage and the cause.
type TransportIRUnloadable struct{ Detail string }

func (e *TransportIRUnloadable) Error() string { return "the IR does not load: " + e.Detail }

// DecodeTransportWorkflow is the program a queue message carries, as every
// consumer reads it: the transport AST decoded and compiled, with the source
// snapshot it travels with. It never prepares the workflow's MCP servers
// (mcp.PrepareWorkflow): the runner prepares them on its own side, and no
// publisher code reads a prepared MCP field.
func DecodeTransportWorkflow(raw []byte) (*ir.Workflow, error) {
	file, err := ast.UnmarshalFile(raw)
	if err != nil {
		return nil, &TransportIRUnloadable{Detail: fmt.Sprintf("decode IR: %v", err)}
	}
	cr := ir.Compile(file)
	if cr.HasErrors() {
		errs := compileErrorDiagnostics(cr.Diagnostics)
		return nil, &TransportIRUnloadable{Detail: fmt.Sprintf("compile IR: %d error(s): %s", len(errs), summariseDiagnostics(errs))}
	}
	cr.Workflow.SourceSnapshot, err = SourceSnapshotFromTransport(raw)
	if err != nil {
		return nil, fmt.Errorf("decode source snapshot: %w", err)
	}
	return cr.Workflow, nil
}

// compileErrorDiagnostics keeps the diagnostics that blocked the load: the
// compiler appends warnings and errors in compile order, so an unfiltered
// head of the list can be three warnings while the error sits further down.
func compileErrorDiagnostics(diags []ir.Diagnostic) []ir.Diagnostic {
	errs := make([]ir.Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Severity == ir.SeverityError {
			errs = append(errs, d)
		}
	}
	return errs
}

// summariseDiagnostics keeps the first few compiler messages for a verdict
// an operator can read on the run, without the whole list.
func summariseDiagnostics(diags []ir.Diagnostic) string {
	parts := make([]string, 0, 3)
	for i, d := range diags {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("… (%d more)", len(diags)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s", d.Code, d.Message))
	}
	return strings.Join(parts, "; ")
}

// TransportWorkflowHash is the workflow hash a transport IR derives: its
// source snapshot compiled with the identity every launch takes
// (compileSourceSnapshot). An IR that carries no snapshot has no hash to
// derive (ErrFreezeUnsupported).
func TransportWorkflowHash(raw []byte) (string, error) {
	source, err := SourceSnapshotFromTransport(raw)
	if err != nil {
		return "", fmt.Errorf("%w: decode its source snapshot: %v", ErrFreezeUnsupported, err)
	}
	if source == nil {
		return "", fmt.Errorf("%w: it carries no source snapshot", ErrFreezeUnsupported)
	}
	_, hash, err := compileSourceSnapshot(source)
	if err != nil {
		return "", fmt.Errorf("%w: compile its source snapshot: %v", ErrFreezeUnsupported, err)
	}
	return hash, nil
}

// FreezeProgram freezes the program a delivery carried for run, for a park
// that source names: the transport IR as bytes (fetched when the message
// names it out of band — never the key, whose object a later publication of
// the run overwrites), its hash derived and compared with the run's, the
// bundle's snapshot stored under its content-addressed key, and the envelope
// written under its own digest. The returned record is what the park writes;
// its QueuedAt is the park's to fill from the document.
func FreezeProgram(ctx context.Context, blobs store.IRBlobStore, msg *queue.RunMessage, run *store.Run, source string) (*store.FrozenProgram, error) {
	if msg == nil || run == nil {
		return nil, errors.New("runview: freeze a program: no message or no run")
	}
	if blobs == nil {
		return nil, fmt.Errorf("%w: run %s: this store keeps no IR blobs", ErrFreezeUnsupported, run.ID)
	}
	raw, err := deliveredIR(ctx, blobs, msg)
	if err != nil {
		return nil, err
	}
	hash, err := TransportWorkflowHash(raw)
	if err != nil {
		return nil, fmt.Errorf("runview: freeze the program of run %s: %w", run.ID, err)
	}
	if hash != run.WorkflowHash {
		return nil, fmt.Errorf("runview: freeze the program of run %s: %w (the delivered IR derives %s, the run reads %s)", run.ID, ErrFreezeHashMismatch, shortHash(hash), shortHash(run.WorkflowHash))
	}
	ref, err := frozenBundleRef(ctx, blobs, run.ID, msg.BotBundle)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(FrozenEnvelope{
		V: frozenEnvelopeVersion, RunID: run.ID, WorkflowName: msg.WorkflowName,
		WorkflowHash: hash, IR: raw, BotBundle: ref,
	})
	if err != nil {
		return nil, fmt.Errorf("runview: encode the frozen program of run %s: %w", run.ID, err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	key, err := blobs.PutIRBlob(ctx, blob.FrozenProgramLogicalKey(run.ID, digest), body)
	if err != nil {
		return nil, fmt.Errorf("runview: store the frozen program of run %s: %w", run.ID, err)
	}
	return &store.FrozenProgram{
		Key: key, Digest: digest, WorkflowHash: hash,
		Source: source, FrozenAt: time.Now().UTC(),
	}, nil
}

// deliveredIR is a copy of the IR msg carries inline, or the bytes its
// out-of-band reference names, read now.
func deliveredIR(ctx context.Context, blobs store.IRBlobStore, msg *queue.RunMessage) ([]byte, error) {
	if len(msg.IRCompiled) > 0 {
		return append([]byte(nil), msg.IRCompiled...), nil
	}
	if msg.IRRef == nil || msg.IRRef.StorageKey == "" {
		return nil, fmt.Errorf("%w: run %s: the message carries neither IRCompiled nor IRRef", ErrFreezeUnsupported, msg.RunID)
	}
	raw, err := blobs.GetIRBlob(ctx, msg.IRRef.StorageKey)
	if err != nil {
		return nil, fmt.Errorf("runview: freeze the program of run %s: fetch its out-of-band IR %s: %w", msg.RunID, msg.IRRef.StorageKey, err)
	}
	return raw, nil
}

// frozenBundleRef is the bundle reference the envelope keeps: the delivered
// one with its snapshot out of band — an inline snapshot stored under the
// same content-addressed key the publisher's offload uses, an out-of-band one
// kept as it is. A legacy reference with no snapshot digest names a stored
// row a later push moves, so it cannot be frozen.
func frozenBundleRef(ctx context.Context, blobs store.IRBlobStore, runID string, ref *queue.BotBundleRef) (*queue.BotBundleRef, error) {
	if ref == nil {
		return nil, nil
	}
	if ref.SnapshotDigest == "" {
		return nil, fmt.Errorf("%w: run %s: its bundle reference (%s/%s v%d) carries no snapshot digest", ErrFreezeUnsupported, runID, ref.TenantID, ref.Slug, ref.Version)
	}
	out := &queue.BotBundleRef{TenantID: ref.TenantID, Slug: ref.Slug, Version: ref.Version, SnapshotDigest: ref.SnapshotDigest}
	switch {
	case len(ref.Snapshot) > 0 && ref.SnapshotRef != nil:
		return nil, fmt.Errorf("%w: run %s: its bundle snapshot is both inline and stored", ErrFreezeUnsupported, runID)
	case len(ref.Snapshot) > 0:
		backend, err := irBackend(blobs)
		if err != nil {
			return nil, err
		}
		key, err := blobs.PutIRBlob(ctx, runID+store.IRBlobFamilyInfix+ref.SnapshotDigest, ref.Snapshot)
		if err != nil {
			return nil, fmt.Errorf("runview: freeze the program of run %s: store its bundle snapshot: %w", runID, err)
		}
		out.SnapshotRef = &queue.IRRef{StorageKey: key, Backend: backend}
	case ref.SnapshotRef != nil:
		stored := *ref.SnapshotRef
		out.SnapshotRef = &stored
	default:
		return nil, fmt.Errorf("%w: run %s: its bundle reference names a snapshot digest and no snapshot", ErrFreezeUnsupported, runID)
	}
	return out, nil
}

// irBackend is the queue's name of the store's IR blob backend.
func irBackend(blobs store.IRBlobStore) (queue.IRBackend, error) {
	switch b := queue.IRBackend(blobs.IRBlobBackend()); b {
	case queue.IRBackendS3, queue.IRBackendMongo:
		return b, nil
	default:
		return "", fmt.Errorf("runview: the IR blob store reports an unsupported backend %q (want s3|mongo)", b)
	}
}

// LoadFrozenProgram reads and verifies the envelope fp names for runID: its
// body hashes to fp.Digest and describes runID and fp.WorkflowHash. A missing
// envelope, or one that fails a check, is ErrFrozenProgramUnavailable; any
// other read error is the store's, returned as is.
func LoadFrozenProgram(ctx context.Context, blobs store.IRBlobStore, runID string, fp *store.FrozenProgram) (*FrozenEnvelope, error) {
	if fp == nil {
		return nil, fmt.Errorf("%w: run %s carries no frozen program", ErrFrozenProgramUnavailable, runID)
	}
	if blobs == nil {
		return nil, fmt.Errorf("%w: run %s: this store keeps no IR blobs", ErrFrozenProgramUnavailable, runID)
	}
	body, err := blobs.GetIRBlob(ctx, fp.Key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: run %s: its envelope %s is gone: %v", ErrFrozenProgramUnavailable, runID, fp.Key, err)
	}
	if err != nil {
		return nil, fmt.Errorf("runview: read the frozen program %s of run %s: %w", fp.Key, runID, err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != fp.Digest {
		return nil, fmt.Errorf("%w: run %s: its envelope %s hashes to %s, the record names %s", ErrFrozenProgramUnavailable, runID, fp.Key, shortHash(got), shortHash(fp.Digest))
	}
	var env FrozenEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: run %s: decode its envelope %s: %v", ErrFrozenProgramUnavailable, runID, fp.Key, err)
	}
	switch {
	case env.V != frozenEnvelopeVersion:
		return nil, fmt.Errorf("%w: run %s: envelope format %d, this server reads %d", ErrFrozenProgramUnavailable, runID, env.V, frozenEnvelopeVersion)
	case env.RunID != runID:
		return nil, fmt.Errorf("%w: run %s: the envelope %s describes run %q", ErrFrozenProgramUnavailable, runID, fp.Key, env.RunID)
	case env.WorkflowHash != fp.WorkflowHash:
		return nil, fmt.Errorf("%w: run %s: the envelope's hash %s is not the record's %s", ErrFrozenProgramUnavailable, runID, shortHash(env.WorkflowHash), shortHash(fp.WorkflowHash))
	case len(env.IR) == 0:
		return nil, fmt.Errorf("%w: run %s: the envelope %s carries no IR", ErrFrozenProgramUnavailable, runID, fp.Key)
	}
	return &env, nil
}

// Bind is the binding re-checked at the moment a re-admission acts on run:
// the run's record still binds it (store.Run.ReadmitFrozen), it is the record
// this re-admission read, and the envelope describes it.
func (f *FrozenResume) Bind(run *store.Run) error {
	if f == nil || f.Envelope == nil {
		return fmt.Errorf("%w: no frozen program to bind", ErrFrozenProgramStale)
	}
	fp, verdict := run.ReadmitFrozen()
	if verdict != store.FrozenBound {
		return fmt.Errorf("%w: run %s reads %s", ErrFrozenProgramStale, run.ID, verdict)
	}
	if fp.Key != f.Program.Key || fp.Digest != f.Program.Digest || !fp.QueuedAt.Equal(f.Program.QueuedAt) {
		return fmt.Errorf("%w: run %s carries another frozen program (%s) than the one read (%s)", ErrFrozenProgramStale, run.ID, fp.Key, f.Program.Key)
	}
	if f.Envelope.RunID != run.ID || f.Envelope.WorkflowHash != fp.WorkflowHash {
		return fmt.Errorf("%w: run %s: the envelope describes run %q at %s", ErrFrozenProgramStale, run.ID, f.Envelope.RunID, shortHash(f.Envelope.WorkflowHash))
	}
	return nil
}

// ResumeNeedsOperator reports a resume refusal only a person can lift: an
// effect left undecided (AMBIGUOUS_EFFECT), a scratch that does not travel
// (SCRATCH_NOT_PORTABLE), a lineage the resume cannot continue
// (RESUME_INVALID), an artifact contract the program no longer meets, a
// required secret that resolves to nothing. Each is a consent or a fix no
// automatic retry gives: the retry sweeper ends its automation on them, named,
// instead of retrying into the same refusal until its budget is spent.
func ResumeNeedsOperator(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, runtime.ErrArtifactContractIncompatible) || errors.Is(err, secrets.ErrRequiredSecretUnresolved) {
		return true
	}
	var rerr *runtime.RuntimeError
	if !errors.As(err, &rerr) {
		return false
	}
	switch rerr.Code {
	case runtime.ErrCodeAmbiguousEffect, runtime.ErrCodeScratchNotPortable, runtime.ErrCodeResumeInvalid:
		return true
	}
	return false
}

// shortHash is a hash cut for a message.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "(none)"
	}
	return h
}
