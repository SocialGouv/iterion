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
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

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
	colInteractions = "interactions"
)

// Config bundles the connection settings for a MongoRunStore.
type Config struct {
	URI            string
	Database       string
	EventsTTLDays  int
	Logger         *iterlog.Logger
	Blob           blob.Client // S3 / blob backend for artifact bodies
}

// Store implements store.RunStore on top of Mongo + a blob backend.
type Store struct {
	client       *mongo.Client
	db           *mongo.Database
	runs         *mongo.Collection
	events       *mongo.Collection
	runSeq       *mongo.Collection
	interactions *mongo.Collection
	blob         blob.Client
	logger       *iterlog.Logger
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

	cli, err := mongo.Connect(options.Client().ApplyURI(cfg.URI))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := cli.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = cli.Disconnect(context.Background())
		return nil, fmt.Errorf("store/mongo: ping: %w", err)
	}

	db := cli.Database(cfg.Database)
	s := &Store{
		client:       cli,
		db:           db,
		runs:         db.Collection(colRuns),
		events:       db.Collection(colEvents),
		runSeq:       db.Collection(colRunSeq),
		interactions: db.Collection(colInteractions),
		blob:         cfg.Blob,
		logger:       cfg.Logger,
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

// Root returns an empty string in cloud mode: the Mongo store has no
// filesystem root to expose. Callers that absolutely need a path
// (engine worktree setup) gate on Capabilities().GitWorktree first.
func (s *Store) Root() string { return "" }

// Capabilities advertises the cloud-store feature set: live events
// will come via Mongo change streams (LiveStream), distributed locks
// are the runner's responsibility (CrossProcessLock), and worktrees
// are not handled at the store level (ephemeral runner clone instead).
func (s *Store) Capabilities() store.Capabilities {
	return store.Capabilities{
		LiveStream:       true,
		CrossProcessLock: true, // implemented in T-26 via NATS KV
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
	// runs collection indexes (plan §D.1).
	_, err := s.runs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: 1}}, Options: options.Index().SetName("status_created")},
		{Keys: bson.D{{Key: "workflow_name", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("workflow_created_desc")},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}, Options: options.Index().SetName("updated_desc")},
		{
			Keys:    bson.D{{Key: "runner_id", Value: 1}},
			Options: options.Index().SetName("runner_id_partial").SetPartialFilterExpression(bson.M{"runner_id": bson.M{"$exists": true, "$ne": nil}}),
		},
	})
	if err != nil && !isIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure runs indexes: %w", err)
	}

	// events collection: unique (run_id, seq) is the race safety net.
	eventIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetUnique(true).SetName("run_seq_unique")},
		{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "type", Value: 1}}, Options: options.Index().SetName("run_type")},
	}
	if eventsTTLDays > 0 {
		// MongoDB requires the TTL to be on a top-level date field.
		// Plan §D.2 names this `ts`.
		eventIdx = append(eventIdx, mongo.IndexModel{
			Keys:    bson.D{{Key: "ts", Value: 1}},
			Options: options.Index().SetName("events_ttl").SetExpireAfterSeconds(int32(eventsTTLDays * 86400)),
		})
	}
	_, err = s.events.Indexes().CreateMany(ctx, eventIdx)
	if err != nil && !isIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure events indexes: %w", err)
	}

	// interactions: query by run_id (the composite _id has run_id as a
	// nested field; an additional index gives us a fast prefix scan).
	_, err = s.interactions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "run_id", Value: 1}},
		Options: options.Index().SetName("run_id"),
	})
	if err != nil && !isIndexConflict(err) {
		return fmt.Errorf("store/mongo: ensure interactions index: %w", err)
	}

	return nil
}

// isIndexConflict matches the error MongoDB returns when an index with
// the same name already exists with different options. We treat these
// as benign on a hot upgrade path — operators recreate them by hand
// when the geometry changes (plan §D.5 forward-only migration).
func isIndexConflict(err error) bool {
	if err == nil {
		return false
	}
	// IndexOptionsConflict (85) and IndexKeySpecsConflict (86) are the
	// usual culprits when re-running EnsureSchema with a different
	// driver version.
	var cmd mongo.CommandError
	if errors.As(err, &cmd) {
		switch cmd.Code {
		case 85, 86:
			return true
		}
	}
	return false
}
