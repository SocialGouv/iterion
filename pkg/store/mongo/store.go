// Package mongo implements the cloud-mode RunStore on top of MongoDB
// for run metadata + events + interactions, paired with an external
// blob.Client (S3) for artifact bodies.
//
// Layout, indexes, and document shapes are spelled out in cloud-ready
// plan §D. Cross-document atomicity guarantees mirror the filesystem
// store wherever it does not require Mongo transactions; the only
// CAS path is SaveCheckpoint → expects optimistic version increment
// (see plan §F T-33).
package mongo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// SchemaVersion is the value MongoRunStore writes into Run.SchemaVersion
// (`v` field) on every persist. Reads of a higher value error out so an
// operator running an older binary against a newer database gets a
// clear "upgrade required" instead of silent data corruption.
const SchemaVersion = 1

// Collection names. Plan §D pins these so monitoring dashboards and
// migration tooling can rely on them.
const (
	colRuns         = "runs"
	colEvents       = "events"
	colRunSeq       = "run_seq"
	colRunLogs      = "run_logs"
	colInteractions = "interactions"
	colUserMessages = "user_messages"
	colRunGitMeta   = "run_gitmeta"
	colRunPlans     = "run_plans"
	colRunNotes     = "run_notes"
	colRunTurns     = "run_turns"
	colRunTags      = "run_tags"
)

// Config bundles the connection settings for a MongoRunStore.
type Config struct {
	URI           string
	Database      string
	EventsTTLDays int
	Logger        *iterlog.Logger
	Blob          blob.Client  // S3 / blob backend for artifact bodies
	LockProvider  LockProvider // optional NATS KV-backed lock; nil → no-op
	// MaxAttachmentBytes caps WriteAttachment payloads in bytes. Zero
	// applies the default (defaultMaxAttachmentBytes). The cap is
	// enforced server-side via io.LimitReader so a malicious or buggy
	// uploader can't push the runner pod into OOM by streaming an
	// arbitrarily large body.
	MaxAttachmentBytes int64
	// RunFilesScratchDir is the runner-local base directory under which
	// EnsureRunFilesDir creates per-run artifact-file scratch areas
	// (bind-mounted into the sandbox). Empty applies the default
	// (<os.TempDir>/iterion-runfiles). Only the runner pod writes here;
	// the server pod reads the uploaded copies from S3.
	RunFilesScratchDir string
}

// defaultMaxAttachmentBytes matches the documented upload cap on the
// runs queue (50 MiB). Increase only after switching WriteAttachment to
// stream into the blob backend instead of buffering into memory.
const defaultMaxAttachmentBytes = 50 * 1024 * 1024

// LockProvider is the abstraction MongoRunStore consults for
// distributed run locks. The runner injects a NATS-KV-backed
// implementation (pkg/queue/nats); the server constructs the store
// without one because it never executes the run itself (locks belong
// to runner pods).
//
// Plan §F T-26.
type LockProvider interface {
	// AcquireLock claims a lease keyed on runID. Returns the abstract
	// store.RunLock the engine consumes; the underlying value also
	// satisfies refresh / release semantics on the lock provider's
	// side (NATS KV TTL refresh).
	AcquireLock(ctx context.Context, runID, runnerID string) (store.RunLock, error)
	// RunnerID returns the identity the provider stamps into each
	// lease record. Surfaced separately so the store can log it on
	// contention without re-resolving the value.
	RunnerID() string
}

// Store implements store.RunStore on top of Mongo + a blob backend.
type Store struct {
	client             *mongo.Client
	db                 *mongo.Database
	runs               *mongo.Collection
	events             *mongo.Collection
	runSeq             *mongo.Collection
	runLogs            *mongo.Collection
	interactions       *mongo.Collection
	userMessages       *mongo.Collection
	runGitMeta         *mongo.Collection
	runPlans           *mongo.Collection
	runNotes           *mongo.Collection
	runTurns           *mongo.Collection
	runTags            *mongo.Collection
	blob               blob.Client
	logger             *iterlog.Logger
	lockProv           LockProvider
	maxAttachmentBytes int64

	// logPositionFn stamps Event.LogOffset at AppendEvent time from the
	// runner's per-run log writer total (the cloud twin of the
	// filesystem store's hook). nil disables stamping. See runlogs.go.
	// runFilesScratch is the runner-local base dir under which
	// EnsureRunFilesDir creates per-run artifact-file scratch areas
	// (see runfiles.go). Empty on a server-only store — it never writes.
	runFilesScratch string

	logPositionMu sync.Mutex
	logPositionFn store.LogPositionFn
	// activeDurationFn stamps Event.ActiveMs at AppendEvent time from the
	// runner's per-run engine monotonic SharedBudget elapsed (the cloud
	// twin of the filesystem store's hook). Guarded by logPositionMu.
	// nil disables stamping. See runlogs.go.
	activeDurationFn store.ActiveDurationFn
}

// Registry returns the BSON codec registry the store's Mongo client is
// constructed with. It overrides the driver's default type map for
// values decoded into `any` — the open-shaped payloads every collection
// carries (Event.Data, Run.Inputs, Checkpoint.Outputs, interaction
// questions/answers, board event payloads, …):
//
//   - embedded documents → map[string]any (driver default: bson.D)
//   - arrays             → []any          (driver default: bson.A)
//   - int32              → int64          (the driver encodes any Go
//     int fitting 32 bits as wire int32, so engine-written counters
//     would otherwise come back as int32)
//
// bson.D / bson.A / int32 are defined types distinct from the
// map[string]any / []any / int-int64-float64 shapes those payloads
// carry on every other path (in-memory engine values, filesystem-store
// JSON, queue/webhook JSON) — and bson.D even marshals to JSON as an
// object, so API responses look right while every Go-side type
// assertion on nested values fails. Consumers shared across stores
// (runview snapshot reducers, subbot output recovery, checkpoint
// reference resolution, expr evaluation, fan-out iteration) assert
// exactly the JSON-ish shapes, so the foreign codec types must never
// cross the store boundary. Normalizing here — on the single client
// all Mongo access (cursors, change streams, DB() consumers) rides —
// gives every collection and every future consumer the same contract.
// Exported so tests decode raw documents exactly the way the store's
// cursors do; guarded by decode_shape_test.go and the storetest
// EventDataDecodeShape conformance case.
func Registry() *bson.Registry {
	reg := bson.NewRegistry()
	doc := reflect.TypeOf(map[string]any{})
	reg.RegisterTypeMapEntry(bson.TypeEmbeddedDocument, doc)
	reg.RegisterTypeMapEntry(bson.Type(0), doc)
	reg.RegisterTypeMapEntry(bson.TypeArray, reflect.TypeOf([]any{}))
	reg.RegisterTypeMapEntry(bson.TypeInt32, reflect.TypeOf(int64(0)))
	return reg
}

// New connects to Mongo, pings to validate credentials, then ensures
// indexes + TTL exist. Returns the live store on success.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.URI == "" {
		return nil, fmt.Errorf("store/mongo: URI is required")
	}
	if cfg.Database == "" {
		cfg.Database = "iterion"
	}
	if cfg.Blob == nil {
		return nil, fmt.Errorf("store/mongo: blob client is required (artifact bodies live in S3)")
	}
	if cfg.Logger == nil {
		cfg.Logger = iterlog.New(iterlog.LevelInfo, nil)
	}

	cli, err := mongo.Connect(options.Client().ApplyURI(cfg.URI).SetRegistry(Registry()))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := cli.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = cli.Disconnect(context.Background())
		return nil, fmt.Errorf("store/mongo: ping: %w", err)
	}

	maxAttach := cfg.MaxAttachmentBytes
	if maxAttach <= 0 {
		maxAttach = defaultMaxAttachmentBytes
	}
	scratch := cfg.RunFilesScratchDir
	if scratch == "" {
		scratch = filepath.Join(os.TempDir(), "iterion-runfiles")
	}
	db := cli.Database(cfg.Database)
	s := &Store{
		client:             cli,
		db:                 db,
		runs:               db.Collection(colRuns),
		events:             db.Collection(colEvents),
		runSeq:             db.Collection(colRunSeq),
		runLogs:            db.Collection(colRunLogs),
		interactions:       db.Collection(colInteractions),
		userMessages:       db.Collection(colUserMessages),
		runGitMeta:         db.Collection(colRunGitMeta),
		runPlans:           db.Collection(colRunPlans),
		runNotes:           db.Collection(colRunNotes),
		runTurns:           db.Collection(colRunTurns),
		runTags:            db.Collection(colRunTags),
		blob:               cfg.Blob,
		logger:             cfg.Logger,
		lockProv:           cfg.LockProvider,
		maxAttachmentBytes: maxAttach,
		runFilesScratch:    scratch,
	}
	if err := s.EnsureSchema(ctx, cfg.EventsTTLDays); err != nil {
		_ = cli.Disconnect(context.Background())
		return nil, err
	}
	return s, nil
}

// Close disconnects the Mongo client. Safe to call multiple times.
func (s *Store) Close(ctx context.Context) error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Disconnect(ctx)
}

// Ping checks the Mongo client can talk to the primary. Used by the
// server's /readyz handler. Caller is expected to wrap in a
// sub-second timeout.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("store/mongo: client not initialised")
	}
	return s.client.Ping(ctx, readpref.Primary())
}

// RunsCollection exposes the underlying Mongo collection so callers
// (e.g. cloudpublisher.queuePosition) can run aggregations the
// store.RunStore interface doesn't surface. Use with care — direct
// access is a layering shortcut, not the long-term API.
func (s *Store) RunsCollection() *mongo.Collection { return s.runs }

// EventsCollection exposes the events collection so the runview
// MongoSource (pkg/runview/runstream/mongo.go) can open change
// streams against the same database the store writes to. Same
// caveat as RunsCollection — short-term shortcut, not the API.
func (s *Store) EventsCollection() *mongo.Collection { return s.events }

// DB exposes the underlying *mongo.Database so adjacent packages
// (pkg/identity, pkg/auth, pkg/secrets) can build their own
// collection handles without re-dialing Mongo. Same caveat as
// RunsCollection — layering shortcut, not the long-term API.
func (s *Store) DB() *mongo.Database { return s.db }

// Root returns an empty string in cloud mode: the Mongo store has no
// filesystem root to expose. Callers that absolutely need a path
// (engine worktree setup) gate on Capabilities().GitWorktree first.
func (s *Store) Root() string { return "" }

// Capabilities advertises the cloud-store feature set: live events
// come via Mongo change streams (LiveStream), distributed locks are
// the runner's responsibility (CrossProcessLock — only true when a
// LockProvider is wired; the server-side store has none and reports
// false so callers don't act on a noop lock), and worktrees are not
// handled at the store level (ephemeral runner clone instead).
func (s *Store) Capabilities() store.Capabilities {
	return store.Capabilities{
		LiveStream:       true,
		CrossProcessLock: s.lockProv != nil,
		PIDFile:          false,
		GitWorktree:      false,
	}
}

// EnsureSchema creates the collections + indexes idempotently so the
// store is safe to bring up against a fresh or already-bootstrapped
// database. Called once on construction; can be re-run safely.
//
// eventsTTLDays==0 disables the TTL.
func (s *Store) EnsureSchema(ctx context.Context, eventsTTLDays int) error {
	// runs collection indexes (plan §D.1). Compound (tenant_id, …)
	// indexes accelerate per-tenant filters; single-field indexes
	// remain for cross-tenant admin views.
	_, err := s.runs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: 1}}, Options: options.Index().SetName("status_created")},
		{Keys: bson.D{{Key: "workflow_name", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("workflow_created_desc")},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}, Options: options.Index().SetName("updated_desc")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "status", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_status_created").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "owner_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_owner_created").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
		// (tenant_id, project_path, created_at desc) backs the "filter
		// runs by repository" studio feature + the distinct-repos
		// aggregation. Partial on project_path so only repo-scoped
		// (webhook-launched) runs index — local/manual runs leave it empty.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "project_path", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_project_created").SetPartialFilterExpression(bson.M{"project_path": bson.M{"$exists": true}})},
		{
			Keys:    bson.D{{Key: "runner_id", Value: 1}},
			Options: options.Index().SetName("runner_id_partial").SetPartialFilterExpression(bson.M{"runner_id": bson.M{"$exists": true}}),
		},
		// Run-tree reverse queries (T4b, refs #125). Partial on the keyed
		// field so only tree-participating runs index — a card-triggered
		// run sets source.issue_id, a shard/child sets parent_run_id;
		// plain manual runs leave both empty.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "source.issue_id", Value: 1}, {Key: "created_at", Value: 1}}, Options: options.Index().SetName("tenant_source_issue_created").SetPartialFilterExpression(bson.M{"source.issue_id": bson.M{"$exists": true}})},
		// Schedule←run reverse edge (pkg/schedgate overlap gate). Partial
		// on source.schedule_id so only schedule-launched runs index.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "source.schedule_id", Value: 1}, {Key: "created_at", Value: 1}}, Options: options.Index().SetName("tenant_source_schedule_created").SetPartialFilterExpression(bson.M{"source.schedule_id": bson.M{"$exists": true}})},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "parent_run_id", Value: 1}, {Key: "created_at", Value: 1}}, Options: options.Index().SetName("tenant_parent_run_created").SetPartialFilterExpression(bson.M{"parent_run_id": bson.M{"$exists": true}})},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure runs indexes: %w", err)
	}

	// events collection: unique (run_id, seq) is the race safety net.
	// (tenant_id, run_id, seq) accelerates change-stream filters
	// without breaking the existing seq-only sort.
	eventIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetUnique(true).SetName("run_seq_unique")},
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "type", Value: 1}}, Options: options.Index().SetName("run_type")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetName("tenant_run_seq").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
	}
	if eventsTTLDays > 0 {
		eventIdx = append(eventIdx, ttlIndexModel("events_ttl", eventsTTLDays))
	}
	_, err = s.events.Indexes().CreateMany(ctx, eventIdx)
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure events indexes: %w", err)
	}

	// run_logs collection (ADR-053): unique (run_id, offset) is the
	// single-writer / redelivery safety net; (tenant_id, run_id, offset)
	// accelerates tenant-scoped range reads + change-stream filters.
	// Log chunks share the events retention knob — both are derived
	// observability streams of the same run.
	runLogIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "offset", Value: 1}}, Options: options.Index().SetUnique(true).SetName("run_offset_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}, {Key: "offset", Value: 1}}, Options: options.Index().SetName("tenant_run_offset").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
	}
	if eventsTTLDays > 0 {
		runLogIdx = append(runLogIdx, ttlIndexModel("run_logs_ttl", eventsTTLDays))
	}
	if _, err := s.runLogs.Indexes().CreateMany(ctx, runLogIdx); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure run_logs indexes: %w", err)
	}

	// interactions: query by run_id (the composite _id has run_id as a
	// nested field; an additional index gives us a fast prefix scan).
	_, err = s.interactions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "run_id", Value: 1}},
		Options: options.Index().SetName("run_id"),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure interactions index: %w", err)
	}

	// user_messages: query by (run_id, status, queued_at) for FIFO
	// drain plus (run_id) for full enumeration.
	_, err = s.userMessages.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "queued_at", Value: 1}}, Options: options.Index().SetName("run_queued")},
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "status", Value: 1}, {Key: "queued_at", Value: 1}}, Options: options.Index().SetName("run_status_queued")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure user_messages indexes: %w", err)
	}

	// run_gitmeta: one doc per run, keyed uniquely by run_id (the runner
	// upserts a whole-snapshot once after the run returns, post-finalize).
	_, err = s.runGitMeta.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "run_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("run_id_unique"),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure run_gitmeta index: %w", err)
	}

	// run_plans: many docs per run (one per captured plan snapshot),
	// uniquely keyed by (run_id, seq) — the race safety net when parallel
	// branches fire TodoWrite concurrently. (tenant_id, run_id, seq)
	// accelerates the tenant-scoped chronological listing. Plan snapshots
	// are a derived observability stream of the run, just like events and
	// run_logs, so they share the same retention knob (eventsTTLDays) on
	// their top-level `ts` date field.
	runPlanIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetUnique(true).SetName("run_seq_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetName("tenant_run_seq").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
	}
	if eventsTTLDays > 0 {
		runPlanIdx = append(runPlanIdx, ttlIndexModel("run_plans_ttl", eventsTTLDays))
	}
	if _, err := s.runPlans.Indexes().CreateMany(ctx, runPlanIdx); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure run_plans indexes: %w", err)
	}

	// run_notes: many docs per run (one per operator note), uniquely keyed
	// by (run_id, seq) — the race safety net when two operators annotate the
	// same run concurrently. (tenant_id, run_id, seq) accelerates the
	// tenant-scoped chronological listing. Notes are durable run annotations,
	// not a derived observability stream, so — unlike events/run_logs/
	// run_plans — they carry NO TTL and persist for the life of the run.
	runNoteIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetUnique(true).SetName("run_seq_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetName("tenant_run_seq").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
	}
	if _, err := s.runNotes.Indexes().CreateMany(ctx, runNoteIdx); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure run_notes indexes: %w", err)
	}

	// run_turns: many docs per run (one per captured LLM turn), uniquely
	// keyed by (run_id, node_id, loop_iter, turn_index) — the idempotent-
	// overwrite key WriteTurn upserts on (the cloud twin of the filesystem
	// store's runs/<id>/turns/<node>/<iter>/<turn>.json). The compound
	// (tenant_id, run_id, node_id, loop_iter, turn_index) accelerates the
	// tenant-scoped per-node listing + LatestTurn/LoadTurnAtIndex sorts.
	// Turn checkpoints are a derived observability stream of the run, like
	// events/run_logs/run_plans, so they share the same retention knob
	// (eventsTTLDays) on their top-level `ts` date field.
	runTurnIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "node_id", Value: 1}, {Key: "loop_iter", Value: 1}, {Key: "turn_index", Value: 1}}, Options: options.Index().SetUnique(true).SetName("run_node_iter_turn_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}, {Key: "node_id", Value: 1}, {Key: "loop_iter", Value: 1}, {Key: "turn_index", Value: 1}}, Options: options.Index().SetName("tenant_run_node_iter_turn").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}})},
	}
	if eventsTTLDays > 0 {
		runTurnIdx = append(runTurnIdx, ttlIndexModel("run_turns_ttl", eventsTTLDays))
	}
	if _, err := s.runTurns.Indexes().CreateMany(ctx, runTurnIdx); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure run_turns indexes: %w", err)
	}

	// run_tags: one doc per run, keyed uniquely by run_id (a whole-list
	// overwrite on every PUT). Operator-assigned filter/group labels are
	// durable metadata, NOT a derived observability stream, so — like
	// run_gitmeta — they carry NO TTL: a tagged run keeps its tags for as
	// long as the run document survives.
	_, err = s.runTags.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "run_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("run_id_unique"),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure run_tags index: %w", err)
	}

	return nil
}

// ttlIndexModel builds a MongoDB TTL index on the top-level `ts` date
// field expiring documents after ttlDays. Shared by the derived
// observability streams (events, run_logs, run_plans) which retain on
// the same eventsTTLDays knob. expireAfterSeconds is an int32, so a very
// large TTL (> ~24855 days) would overflow the cast to a negative value;
// clamp to int32 max (~68 years) instead.
func ttlIndexModel(name string, ttlDays int) mongo.IndexModel {
	secs := int64(ttlDays) * 86400
	const maxTTLSeconds = int64(1<<31 - 1)
	if secs > maxTTLSeconds {
		secs = maxTTLSeconds
	}
	return mongo.IndexModel{
		Keys:    bson.D{{Key: "ts", Value: 1}},
		Options: options.Index().SetName(name).SetExpireAfterSeconds(int32(secs)),
	}
}
