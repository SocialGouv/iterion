package runview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// memIRBlobs is an IR blob store that replaces an object written again under
// the same key — S3's behaviour on ir/<run>.json — and fails on demand.
type memIRBlobs struct {
	mu      sync.Mutex
	data    map[string][]byte
	putErr  error
	getErr  error
	backend string
}

func newMemIRBlobs() *memIRBlobs { return &memIRBlobs{data: map[string][]byte{}, backend: "s3"} }

func (m *memIRBlobs) PutIRBlob(_ context.Context, id string, body []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return "", m.putErr
	}
	key := "ir/" + id + ".json"
	m.data[key] = append([]byte(nil), body...)
	return key, nil
}

func (m *memIRBlobs) GetIRBlob(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	body, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("ir blob %s: %w", key, os.ErrNotExist)
	}
	return append([]byte(nil), body...), nil
}

func (m *memIRBlobs) IRBlobBackend() string { return m.backend }

func (m *memIRBlobs) set(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = append([]byte(nil), body...)
}

const frozenV1 = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\nworkflow frozen:\n  entry: one\n  one -> done\n"
const frozenV2 = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\ntool two:\n  command: `true`\n  output: out\n\nworkflow frozen:\n  entry: one\n  one -> two\n  two -> done\n"

// transportOf is the IR a launch of src publishes (marshalAttemptIR's
// snapshot branch) and the hash the launch stamps on the run.
func transportOf(t *testing.T, src string) ([]byte, string) {
	t.Helper()
	wf, cs, _, err := compileForLaunch("", src, "", "", "", store.RunTrustDefault)
	if err != nil {
		t.Fatalf("compileForLaunch: %v", err)
	}
	body, err := MarshalSourceSnapshot(wf.SourceSnapshot)
	if err != nil {
		t.Fatalf("MarshalSourceSnapshot: %v", err)
	}
	return body, cs.Hash
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// F7. The gate the whole design rests on: the hash a transport IR derives is
// the hash its launch stamped on the run, for every shape a launch publishes
// — an inline source, a stored bundle with prompts, a prompt include and an
// imported fragment, a baked bot (a path promoted to its bundle), and a
// sibling entry of a bundle (a subbot's). A shape that does not derive it
// would freeze as hash_mismatch. Red when the derivation hashes the main
// file alone.
func TestTransportWorkflowHashIsTheLaunchHash(t *testing.T) {
	bundleFiles := map[string]string{
		"main.bot":           "import \"lib/nodes.bot\"\n\nschema out:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: out\n  system: helper\n\nworkflow w:\n  entry: a\n  a -> t\n  t -> done\n",
		"lib/nodes.bot":      "tool t:\n  command: `true`\n  output: out\n",
		"prompts/helper.md":  "Helper.\n{{include \"sibling.md\"}}\n",
		"prompts/sibling.md": "Sibling text.\n",
		"child.bot":          "schema out:\n  ok: bool\n\ntool c:\n  command: `true`\n  output: out\n\nworkflow child:\n  entry: c\n  c -> done\n",
		bundle.ManifestFile:  "name: frozen-sample\nschema_version: 1\n",
		"skills/s.md":        "A skill.\n",
	}
	shapes := []struct {
		name string
		load func(t *testing.T) (*ir.Workflow, *CompiledSource)
	}{
		{"inline source", func(t *testing.T) (*ir.Workflow, *CompiledSource) {
			wf, cs, _, err := compileForLaunch("", frozenV1, "", "", "", store.RunTrustDefault)
			if err != nil {
				t.Fatal(err)
			}
			return wf, cs
		}},
		{"stored bundle with prompts, an include and a fragment", func(t *testing.T) (*ir.Workflow, *CompiledSource) {
			dir := t.TempDir()
			writeTree(t, dir, bundleFiles)
			wf, cs, _, err := compileForLaunch(filepath.Join(dir, "main.bot"), bundleFiles["main.bot"], dir, "", "", store.RunTrustDefault)
			if err != nil {
				t.Fatal(err)
			}
			return wf, cs
		}},
		{"baked bot promoted from its path", func(t *testing.T) (*ir.Workflow, *CompiledSource) {
			dir := t.TempDir()
			writeTree(t, dir, bundleFiles)
			wf, cs, b, err := compileForLaunch(filepath.Join(dir, "main.bot"), "", "", "", "", store.RunTrustDefault)
			if err != nil || b == nil {
				t.Fatalf("baked: bundle %v err %v", b, err)
			}
			return wf, cs
		}},
		{"sibling entry of a bundle", func(t *testing.T) (*ir.Workflow, *CompiledSource) {
			dir := t.TempDir()
			writeTree(t, dir, bundleFiles)
			wf, cs, _, err := compileForLaunch(filepath.Join(dir, "child.bot"), bundleFiles["child.bot"], dir, "child.bot", "", store.RunTrustDefault)
			if err != nil {
				t.Fatal(err)
			}
			return wf, cs
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			wf, cs := sh.load(t)
			if cs.Hash == "" || wf.SourceSnapshot == nil {
				t.Fatalf("precondition: hash %q snapshot %v", cs.Hash, wf.SourceSnapshot)
			}
			body, err := MarshalSourceSnapshot(wf.SourceSnapshot)
			if err != nil {
				t.Fatalf("MarshalSourceSnapshot: %v", err)
			}
			got, err := TransportWorkflowHash(body)
			if err != nil {
				t.Fatalf("TransportWorkflowHash: %v", err)
			}
			if got != cs.Hash {
				t.Fatalf("the transport derives %s, the launch stamped %s", got, cs.Hash)
			}
		})
	}
	if _, err := TransportWorkflowHash([]byte(`{"workflows":[]}`)); !errors.Is(err, ErrFreezeUnsupported) {
		t.Fatalf("an IR with no source snapshot: %v, want ErrFreezeUnsupported", err)
	}
}

func frozenRun(id, hash string) *store.Run {
	q := time.Now().UTC().Truncate(time.Millisecond)
	return &store.Run{ID: id, WorkflowHash: hash, QueuedAt: &q, Checkpoint: &store.Checkpoint{NodeID: "one"}}
}

// F8. An IR the message names out of band is frozen as bytes: a later
// publication of the run replaces ir/<run>.json, and the envelope still holds
// the program the parked delivery carried. Red when the envelope keeps the
// key instead of the bytes.
func TestFreezeProgram_AnIRRefIsFrozenAsBytes(t *testing.T) {
	blobs := newMemIRBlobs()
	v1, h1 := transportOf(t, frozenV1)
	v2, _ := transportOf(t, frozenV2)
	key, _ := blobs.PutIRBlob(context.Background(), "run-a", v1)
	msg := &queue.RunMessage{RunID: "run-a", WorkflowName: "frozen", WorkflowHash: h1, IRRef: &queue.IRRef{StorageKey: key, Backend: queue.IRBackendS3}}
	run := frozenRun("run-a", h1)
	fp, err := FreezeProgram(context.Background(), blobs, msg, run, store.FrozenProgramSourceCapacity)
	if err != nil {
		t.Fatalf("FreezeProgram: %v", err)
	}
	blobs.set(key, v2)
	env, err := LoadFrozenProgram(context.Background(), blobs, "run-a", fp)
	if err != nil {
		t.Fatalf("LoadFrozenProgram: %v", err)
	}
	if string(env.IR) != string(v1) {
		t.Fatalf("the envelope holds %d bytes that are not the delivered v1 IR", len(env.IR))
	}
	if !strings.HasPrefix(fp.Key, "ir/run-a-bundle-program-") || fp.WorkflowHash != h1 || fp.Source != store.FrozenProgramSourceCapacity {
		t.Fatalf("record = %+v", fp)
	}
}

// F9. An IR the run's key already names in another revision — a later
// publication overwrote it, then its flip was reverted — derives another hash
// than the run's: the freeze refuses it as a hash mismatch rather than
// freezing a program the run never ran. Red when the freeze compares with the
// message's hash.
func TestFreezeProgram_AnIRAlreadyReplacedIsAHashMismatch(t *testing.T) {
	blobs := newMemIRBlobs()
	_, h1 := transportOf(t, frozenV1)
	v2, _ := transportOf(t, frozenV2)
	key, _ := blobs.PutIRBlob(context.Background(), "run-a", v2)
	msg := &queue.RunMessage{RunID: "run-a", WorkflowName: "frozen", WorkflowHash: h1, IRRef: &queue.IRRef{StorageKey: key, Backend: queue.IRBackendS3}}
	_, err := FreezeProgram(context.Background(), blobs, msg, frozenRun("run-a", h1), store.FrozenProgramSourceCapacity)
	if !errors.Is(err, ErrFreezeHashMismatch) {
		t.Fatalf("FreezeProgram over a replaced IR: %v, want ErrFreezeHashMismatch", err)
	}
}

// snapshotBundle is a stored bundle's snapshot as a launch carries it.
func snapshotBundle(t *testing.T) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"bot/main.bot": frozenV1, "bot/skills/s.md": "skill"})
	s := &bundle.Snapshot{Root: "bot"}
	if err := s.AddDir("bot", filepath.Join(dir, "bot")); err != nil {
		t.Fatal(err)
	}
	body, digest, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return body, digest
}

// F10. An inline bundle snapshot is stored under the content-addressed key
// the publisher's offload uses, and the envelope keeps only its reference —
// from which the runner's materialization decodes the same snapshot. A legacy
// reference with no digest cannot be frozen. Red when the snapshot is not
// stored.
func TestFreezeProgram_AnInlineSnapshotIsStoredOutOfBand(t *testing.T) {
	blobs := newMemIRBlobs()
	v1, h1 := transportOf(t, frozenV1)
	snap, digest := snapshotBundle(t)
	msg := &queue.RunMessage{RunID: "run-a", WorkflowName: "frozen", WorkflowHash: h1, IRCompiled: v1,
		BotBundle: &queue.BotBundleRef{TenantID: "team-1", Slug: "bot", Version: 3, Snapshot: snap, SnapshotDigest: digest}}
	fp, err := FreezeProgram(context.Background(), blobs, msg, frozenRun("run-a", h1), store.FrozenProgramSourceCapacity)
	if err != nil {
		t.Fatalf("FreezeProgram: %v", err)
	}
	env, err := LoadFrozenProgram(context.Background(), blobs, "run-a", fp)
	if err != nil {
		t.Fatalf("LoadFrozenProgram: %v", err)
	}
	ref := env.BotBundle
	if ref == nil || len(ref.Snapshot) != 0 || ref.SnapshotRef == nil || ref.SnapshotDigest != digest || ref.Slug != "bot" || ref.Version != 3 {
		t.Fatalf("envelope bundle ref = %+v, want the reference alone", ref)
	}
	if ref.SnapshotRef.StorageKey != "ir/run-a-bundle-"+digest+".json" {
		t.Fatalf("snapshot stored at %q, want the offload's content-addressed key", ref.SnapshotRef.StorageKey)
	}
	body, err := blobs.GetIRBlob(context.Background(), ref.SnapshotRef.StorageKey)
	if err != nil {
		t.Fatalf("the snapshot the envelope names is not stored: %v", err)
	}
	if _, err := bundle.DecodeSnapshot(body, digest); err != nil {
		t.Fatalf("the stored snapshot does not decode: %v", err)
	}
	if msg.BotBundle.Snapshot == nil {
		t.Fatal("the freeze emptied the delivered message's snapshot")
	}
	legacy := *msg
	legacy.BotBundle = &queue.BotBundleRef{TenantID: "team-1", Slug: "bot", Version: 3}
	if _, err := FreezeProgram(context.Background(), blobs, &legacy, frozenRun("run-a", h1), store.FrozenProgramSourceCapacity); !errors.Is(err, ErrFreezeUnsupported) {
		t.Fatalf("a digest-less bundle reference: %v, want ErrFreezeUnsupported", err)
	}
}

// F11 and F12. The envelope holds the program and nothing an attempt owns —
// no capacity holds, no sealed secrets or their key, no resume patch — and
// the IR byte for byte, pretty-printing included. Red when the envelope
// carries attempt fields, or keeps the IR as a raw JSON message (compacted
// on the way through).
func TestFrozenEnvelope_CarriesTheProgramOnlyByteForByte(t *testing.T) {
	blobs := newMemIRBlobs()
	compact, h1 := transportOf(t, frozenV1)
	var pretty json.RawMessage
	{
		var doc any
		if err := json.Unmarshal(compact, &doc); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(doc, "", "    ")
		if err != nil {
			t.Fatal(err)
		}
		pretty = b
	}
	msg := &queue.RunMessage{RunID: "run-a", WorkflowName: "frozen", WorkflowHash: h1, IRCompiled: pretty,
		SecretsRef: "secrets/run-a", BundleDEK: []byte("dek"),
		Capacity: &queue.CapacityAdmission{Holds: []store.CapacityHold{{Subject: "fp", Token: time.Now()}}},
		Resume:   &queue.ResumeSpec{AcceptScratchLoss: true}}
	fp, err := FreezeProgram(context.Background(), blobs, msg, frozenRun("run-a", h1), store.FrozenProgramSourceCapacity)
	if err != nil {
		t.Fatalf("FreezeProgram: %v", err)
	}
	raw, err := blobs.GetIRBlob(context.Background(), fp.Key)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := "ir run_id v workflow_hash workflow_name"; strings.Join(got, " ") != want {
		t.Fatalf("envelope keys %v, want exactly %q", got, want)
	}
	env, err := LoadFrozenProgram(context.Background(), blobs, "run-a", fp)
	if err != nil {
		t.Fatalf("LoadFrozenProgram: %v", err)
	}
	if string(env.IR) != string(pretty) {
		t.Fatalf("the envelope's IR is not the delivered bytes:\n got %q\nwant %q", env.IR[:min(80, len(env.IR))], pretty[:80])
	}
}

// F13. The reader trusts nothing it cannot verify: a body that does not hash
// to the record's digest, or an envelope that is gone, is unavailable; any
// other read failure is the store's, never taken for a missing envelope. Red
// when the digest check is skipped.
func TestLoadFrozenProgram_RefusesWhatItCannotVerify(t *testing.T) {
	blobs := newMemIRBlobs()
	v1, h1 := transportOf(t, frozenV1)
	msg := &queue.RunMessage{RunID: "run-a", WorkflowName: "frozen", WorkflowHash: h1, IRCompiled: v1}
	fp, err := FreezeProgram(context.Background(), blobs, msg, frozenRun("run-a", h1), store.FrozenProgramSourceCapacity)
	if err != nil {
		t.Fatalf("FreezeProgram: %v", err)
	}
	raw, _ := blobs.GetIRBlob(context.Background(), fp.Key)
	var env FrozenEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env.WorkflowName = "tampered"
	tampered, _ := json.Marshal(env)
	blobs.set(fp.Key, tampered)
	if _, err := LoadFrozenProgram(context.Background(), blobs, "run-a", fp); !errors.Is(err, ErrFrozenProgramUnavailable) {
		t.Fatalf("a tampered envelope: %v, want ErrFrozenProgramUnavailable", err)
	}
	gone := *fp
	gone.Key = "ir/run-a-bundle-program-absent.json"
	if _, err := LoadFrozenProgram(context.Background(), blobs, "run-a", &gone); !errors.Is(err, ErrFrozenProgramUnavailable) {
		t.Fatalf("a missing envelope: %v, want ErrFrozenProgramUnavailable", err)
	}
	blobs.set(fp.Key, raw)
	blobs.getErr = errors.New("s3: connection reset")
	if _, err := LoadFrozenProgram(context.Background(), blobs, "run-a", fp); err == nil || errors.Is(err, ErrFrozenProgramUnavailable) {
		t.Fatalf("a store failure: %v, want the store's error, not unavailable", err)
	}
}

// frozenStore is a run store whose runs carry a frozen program — the field
// the filesystem twin never persists, as only the Mongo park writes it.
type frozenStore struct {
	store.RunStore
	mu sync.Mutex
	fp map[string]*store.FrozenProgram
}

func (s *frozenStore) Unwrap() store.RunStore { return s.RunStore }

func (s *frozenStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if err != nil || r == nil {
		return r, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if fp := s.fp[id]; fp != nil {
		cp := *fp
		r.FrozenProgram = &cp
	}
	return r, nil
}

// capturingPublisher records the resumes it is handed.
type capturingPublisher struct {
	operatorResumePublisher
	spec ResumeSpec
	wf   *ir.Workflow
	cs   *CompiledSource
}

func (p *capturingPublisher) SubmitResume(_ context.Context, spec ResumeSpec, wf *ir.Workflow, cs *CompiledSource) error {
	p.resumeCalls++
	p.spec, p.wf, p.cs = spec, wf, cs
	return nil
}

// frozenResumeFixture is a started run, parked failed_resumable on the v1
// program, whose bot has since become v2, with its frozen program read back.
func frozenResumeFixture(t *testing.T, publisher LaunchPublisher) (*Service, *frozenStore, *FrozenResume) {
	t.Helper()
	fs, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	st := &frozenStore{RunStore: fs, fp: map[string]*store.FrozenProgram{}}
	opts := []ServiceOption{WithLogger(iterlog.Nop()), WithStore(st)}
	if publisher != nil {
		opts = append(opts, WithLaunchPublisher(publisher))
	}
	svc, err := NewService(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v1, h1 := transportOf(t, frozenV1)
	if _, err := fs.CreateRun(ctx, "run-a", "frozen", nil); err != nil {
		t.Fatal(err)
	}
	r, err := fs.LoadRun(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	q := time.Now().UTC().Truncate(time.Millisecond)
	r.WorkflowHash, r.QueuedAt, r.FilePath = h1, &q, "/nonexistent/bots/frozen/main.bot"
	if err := fs.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := fs.SaveCheckpoint(ctx, "run-a", &store.Checkpoint{NodeID: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.UpdateRunStatusCoded(ctx, "run-a", store.RunStatusFailedResumable, "parked: place lost", store.FailureCredSlotWait); err != nil {
		t.Fatal(err)
	}
	blobs := newMemIRBlobs()
	run, err := fs.LoadRun(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := FreezeProgram(ctx, blobs, &queue.RunMessage{RunID: "run-a", WorkflowName: "frozen", WorkflowHash: h1, IRCompiled: v1}, run, store.FrozenProgramSourceCapacity)
	if err != nil {
		t.Fatalf("FreezeProgram: %v", err)
	}
	fp.QueuedAt = *run.QueuedAt
	st.fp["run-a"] = fp
	env, err := LoadFrozenProgram(ctx, blobs, "run-a", fp)
	if err != nil {
		t.Fatalf("LoadFrozenProgram: %v", err)
	}
	return svc, st, &FrozenResume{Program: *fp, Envelope: env}
}

// A frozen re-admission republishes the parked program, not the bot's
// current one: no file is resolved (its path names nothing on this server),
// the publisher is handed the envelope's program and identity, never forced.
// A record that no longer binds the run — its attempt moved — is refused
// before anything is published, and a service with no publisher refuses it.
func TestResumeFrozen_RepublishesTheParkedProgram(t *testing.T) {
	pub := &capturingPublisher{}
	svc, st, frozen := frozenResumeFixture(t, pub)
	ctx := context.Background()
	if _, err := svc.Resume(ctx, ResumeSpec{RunID: "run-a", Automatic: true, Frozen: frozen}); err != nil {
		t.Fatalf("frozen Resume: %v", err)
	}
	if pub.resumeCalls != 1 || pub.spec.Frozen == nil || pub.spec.Force {
		t.Fatalf("publisher calls %d frozen %v force %v, want one unforced frozen republish", pub.resumeCalls, pub.spec.Frozen != nil, pub.spec.Force)
	}
	if pub.cs == nil || pub.cs.Hash != frozen.Envelope.WorkflowHash || len(pub.cs.Files) != 0 {
		t.Fatalf("compiled source %+v, want the envelope's identity and no files", pub.cs)
	}
	if _, ok := pub.wf.Nodes["two"]; ok {
		t.Fatal("the republished program has v2's node: the current bot was compiled")
	}
	if _, ok := pub.wf.Nodes["one"]; !ok {
		t.Fatal("the republished program lacks the parked program's node")
	}

	moved := frozen.Program
	moved.QueuedAt = moved.QueuedAt.Add(time.Second)
	st.mu.Lock()
	st.fp["run-a"] = &moved
	st.mu.Unlock()
	if _, err := svc.Resume(ctx, ResumeSpec{RunID: "run-a", Automatic: true, Frozen: frozen}); !errors.Is(err, ErrFrozenProgramStale) {
		t.Fatalf("a record that no longer binds: %v, want ErrFrozenProgramStale", err)
	}
	if pub.resumeCalls != 1 {
		t.Fatalf("a stale re-admission published (%d calls)", pub.resumeCalls)
	}

	local, _, lf := frozenResumeFixture(t, nil)
	if _, err := local.Resume(ctx, ResumeSpec{RunID: "run-a", Automatic: true, Frozen: lf}); !errors.Is(err, ErrFrozenResumeNotCloud) {
		t.Fatalf("a frozen resume with no publisher: %v, want ErrFrozenResumeNotCloud", err)
	}
}

// frozenBroken is the frozen bot pushed into a program that does not compile:
// an edge to a node it does not declare.
const frozenBroken = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\nworkflow frozen:\n  entry: one\n  one -> missing\n"

// A resume of the bot's current program that does not compile is refused
// before anything is published, typed ErrCurrentProgramUnloadable with the
// compile refusal's own text and chain — the retry sweeper reads it as a
// bot that changed during a usage wait. A current program that compiles to
// another hash is refused for its changed source and is not typed so. Red
// when the compile refusal is left untyped (the sweeper re-arms it against
// the budget), or when the type is put on every refusal.
func TestResume_ACurrentProgramThatDoesNotCompileIsTyped(t *testing.T) {
	pub := &capturingPublisher{}
	svc, _, _ := frozenResumeFixture(t, pub)
	ctx := context.Background()
	_, _, _, compileErr := compileForLaunch("main.bot", frozenBroken, "", "", "", store.RunTrustDefault)
	if compileErr == nil {
		t.Fatal("precondition: the broken program compiles")
	}
	_, err := svc.Resume(ctx, ResumeSpec{RunID: "run-a", Automatic: true, FilePath: "main.bot", Source: frozenBroken})
	if !errors.Is(err, ErrCurrentProgramUnloadable) {
		t.Fatalf("a current program that does not compile: %v, want ErrCurrentProgramUnloadable", err)
	}
	if err.Error() != compileErr.Error() {
		t.Fatalf("the refusal reads %q, want the compile refusal's own text %q", err, compileErr)
	}
	if errors.Is(err, runtime.ErrWorkflowSourceChanged) || ResumeNeedsOperator(err) || RunMayHaveStarted(err) {
		t.Fatalf("the compile refusal is classed as a source change, a consent refusal or a started resume: %v", err)
	}
	_, err = svc.Resume(ctx, ResumeSpec{RunID: "run-a", Automatic: true, FilePath: "main.bot", Source: frozenV2})
	if !errors.Is(err, runtime.ErrWorkflowSourceChanged) || errors.Is(err, ErrCurrentProgramUnloadable) {
		t.Fatalf("a current program that compiles to another hash: %v, want the source change alone", err)
	}
	if pub.resumeCalls != 0 {
		t.Fatalf("a refused resume published (%d calls)", pub.resumeCalls)
	}
}

// F14. The frozen branch skips the source and nothing else: a lineage that
// does not travel is refused as it is on every resume — before the
// publisher, with RESUME_INVALID — though the run's file names nothing on
// this server and no source is given. Red when the frozen branch skips the
// lineage check.
func TestResumeFrozen_KeepsTheLineageRefusal(t *testing.T) {
	pub := &capturingPublisher{}
	svc, st, frozen := frozenResumeFixture(t, pub)
	ctx := context.Background()
	r, err := st.RunStore.LoadRun(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	r.ParentRunID = "run-parent"
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, "run-a", store.Event{Type: store.EventSandboxShared, Data: map[string]any{
		"adopted": true, "driver": "kubernetes", "parent_run": "run-parent", "copy_based": true, "scratch_container_local": false,
	}}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Resume(ctx, ResumeSpec{RunID: "run-a", Automatic: true, Frozen: frozen})
	var rerr *runtime.RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != runtime.ErrCodeResumeInvalid {
		t.Fatalf("a frozen resume over a lineage that does not travel: %v, want RESUME_INVALID", err)
	}
	if pub.resumeCalls != 0 {
		t.Fatalf("the refused resume published (%d calls)", pub.resumeCalls)
	}
	if !ResumeNeedsOperator(err) {
		t.Fatalf("the lineage refusal is not classed as needing an operator: %v", err)
	}
}

// ResumeNeedsOperator classes the consent refusals and nothing else: a
// changed source, a transient, are retried by automation. Red when a class
// is dropped, or the hash refusal is classed with them.
func TestResumeNeedsOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"undecided effect", runtime.CheckApprovalReplay(&store.Run{UnresolvedToolApproval: "tool-1"}), true},
		{"scratch", fmt.Errorf("wrapped: %w", &runtime.RuntimeError{Code: runtime.ErrCodeScratchNotPortable, Message: "gone"}), true},
		{"lineage", &runtime.RuntimeError{Code: runtime.ErrCodeResumeInvalid, Message: "lineage"}, true},
		{"artifact contract", fmt.Errorf("x: %w", runtime.ErrArtifactContractIncompatible), true},
		{"required secret", fmt.Errorf("cloudpublisher: %w", secrets.RequiredSecretsError([]string{"tok"}, "this team/bot")), true},
		{"source changed", runtime.ValidateResumeWorkflowHash("run-a", "h1", "h2", false), false},
		{"other runtime code", &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: "boom"}, false},
		{"transient", errors.New("publish: nats timeout"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResumeNeedsOperator(tc.err); got != tc.want {
				t.Fatalf("ResumeNeedsOperator(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The verdict names the errors that blocked the load, not the warnings the
// compiler happened to append first.
func TestSummariseDiagnostics_ErrorsOnly(t *testing.T) {
	diags := []ir.Diagnostic{
		{Code: "C089", Severity: ir.SeverityWarning, Message: "ultracode on a model that is not opus"},
		{Code: "C128", Severity: ir.SeverityWarning, Message: "sandbox note"},
		{Code: "C001", Severity: ir.SeverityError, Message: "unknown node kind"},
	}
	errs := compileErrorDiagnostics(diags)
	if len(errs) != 1 || errs[0].Code != "C001" {
		t.Fatalf("compileErrorDiagnostics = %v, want the single error", errs)
	}
	if got := summariseDiagnostics(errs); !strings.Contains(got, "C001") || strings.Contains(got, "C089") {
		t.Fatalf("summary = %q, want the error named and the warnings absent", got)
	}
}
