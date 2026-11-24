package pricefeed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The Mongo twins of the feed's durable state. The config store mirrors
// platformcfg's scoped-store semantics (ReplaceOne upsert so a cleared
// feed really disappears, CAS on updated_at); the state store is the
// reconciler's single-writer surface.

// MongoConfigStore is the cloud ConfigStore.
type MongoConfigStore struct{ col *mongo.Collection }

// NewMongoConfigStore binds the feed-list collection to a database.
func NewMongoConfigStore(db *mongo.Database) *MongoConfigStore {
	return &MongoConfigStore{col: db.Collection(ColConfigs)}
}

// configDoc carries the document id beside the record — List's decode.
type configDoc struct {
	ID           string `bson:"_id"`
	ConfigRecord `bson:",inline"`
}

func configToDoc(docID string, rec ConfigRecord) (bson.M, error) {
	body, err := bson.Marshal(configDoc{ID: docID, ConfigRecord: rec})
	if err != nil {
		return nil, fmt.Errorf("pricefeed: encode %s: %w", docID, err)
	}
	var doc bson.M
	if err := bson.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("pricefeed: encode %s: %w", docID, err)
	}
	doc["_id"] = docID
	return doc, nil
}

// Get answers one config document, nil when never written.
func (s *MongoConfigStore) Get(ctx context.Context, docID string) (*ConfigRecord, error) {
	var doc configDoc
	err := s.col.FindOne(ctx, bson.M{"_id": docID}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricefeed: get %s: %w", docID, err)
	}
	rec := doc.ConfigRecord
	return &rec, nil
}

// Put writes one config document unconditionally. It still goes through
// the CAS so the stored token advances past whatever it replaces.
func (s *MongoConfigStore) Put(ctx context.Context, docID string, rec *ConfigRecord) error {
	// An unconditional replacement still needs a new token. Retry the CAS
	// if another writer advanced it after our read; an unguarded ReplaceOne
	// would let two Put callers publish the same successor stamp.
	for {
		previous, err := s.Get(ctx, docID)
		if err != nil {
			return err
		}
		var stamp time.Time
		if previous != nil {
			stamp = previous.UpdatedAt
		}
		ok, err := s.PutIfUnchanged(ctx, docID, rec, stamp)
		if err != nil || ok {
			return err
		}
	}
}

// configCASFilter: a zero stamp matches the shapes this store reads as
// time.Time{} and the zero-stamp CAS accepts: field missing, null, or the
// zero DateTime. A nonzero stamp must match the stored DateTime exactly.
// The _id index arbitrates first inserts.
func configCASFilter(docID string, stamp time.Time) bson.M {
	if stamp.IsZero() {
		return bson.M{"_id": docID, "$or": bson.A{
			bson.M{"updated_at": nil}, // null also matches an absent field
			bson.M{"updated_at": stamp},
		}}
	}
	return bson.M{"_id": docID, "updated_at": stamp}
}

// PutIfUnchanged writes rec only when the stored updated_at still equals
// prevUpdatedAt (zero = no document, or one whose updated_at is missing,
// null or the zero date). The stamp lands on a copy: the caller's record
// stays unwritten, the way the memory twin keeps it.
func (s *MongoConfigStore) PutIfUnchanged(ctx context.Context, docID string, rec *ConfigRecord, prevUpdatedAt time.Time) (bool, error) {
	stamped := *rec
	stamped.UpdatedAt = nextConfigStamp(prevUpdatedAt, time.Now(), time.Millisecond)
	doc, err := configToDoc(docID, stamped)
	if err != nil {
		return false, err
	}
	res, err := s.col.ReplaceOne(ctx, configCASFilter(docID, prevUpdatedAt), doc, options.Replace().SetUpsert(prevUpdatedAt.IsZero()))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, fmt.Errorf("pricefeed: put %s: %w", docID, err)
	}
	return res.MatchedCount > 0 || res.UpsertedCount > 0, nil
}

// DeleteIfUnchanged removes the document when the stamp still matches.
func (s *MongoConfigStore) DeleteIfUnchanged(ctx context.Context, docID string, prevUpdatedAt time.Time) (bool, error) {
	res, err := s.col.DeleteOne(ctx, configCASFilter(docID, prevUpdatedAt))
	if err != nil {
		return false, fmt.Errorf("pricefeed: delete %s: %w", docID, err)
	}
	return res.DeletedCount > 0, nil
}

// List answers every config document by id — the reconciler's walk.
func (s *MongoConfigStore) List(ctx context.Context) (map[string]*ConfigRecord, error) {
	cursor, err := s.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("pricefeed: list configs: %w", err)
	}
	out := map[string]*ConfigRecord{}
	for cursor.Next(ctx) {
		var doc configDoc
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("pricefeed: list configs: %w", err)
		}
		rec := doc.ConfigRecord
		out[doc.ID] = &rec
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("pricefeed: list configs: %w", err)
	}
	return out, nil
}

// MongoStateStore is the cloud StateStore.
type MongoStateStore struct{ col *mongo.Collection }

// NewMongoStateStore binds the feed-state collection to a database.
func NewMongoStateStore(db *mongo.Database) *MongoStateStore {
	return &MongoStateStore{col: db.Collection(ColStates)}
}

// stateDoc carries the document id beside the record — List's decode.
type stateDoc struct {
	ID          string `bson:"_id"`
	StateRecord `bson:",inline"`
}

// Get answers one state document, nil when never written.
func (s *MongoStateStore) Get(ctx context.Context, docID string) (*StateRecord, error) {
	var doc stateDoc
	err := s.col.FindOne(ctx, bson.M{"_id": docID}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricefeed: get state %s: %w", docID, err)
	}
	rec := doc.StateRecord
	return &rec, nil
}

// stateToDoc encodes one state document the way Put writes it — the
// record inline beside its id, through the driver's default codec.
func stateToDoc(docID string, rec StateRecord) (bson.M, error) {
	body, err := bson.Marshal(stateDoc{ID: docID, StateRecord: rec})
	if err != nil {
		return nil, fmt.Errorf("pricefeed: encode state %s: %w", docID, err)
	}
	var doc bson.M
	if err := bson.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("pricefeed: encode state %s: %w", docID, err)
	}
	doc["_id"] = docID
	return doc, nil
}

// Put writes one state document, stamping UpdatedAt on a copy: the
// caller's record stays unwritten, the way the memory twin keeps it.
func (s *MongoStateStore) Put(ctx context.Context, docID string, rec *StateRecord) error {
	stamped := *rec
	stamped.UpdatedAt = time.Now().UTC()
	doc, err := stateToDoc(docID, stamped)
	if err != nil {
		return err
	}
	if _, err := s.col.ReplaceOne(ctx, bson.M{"_id": docID}, doc, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("pricefeed: put state %s: %w", docID, err)
	}
	return nil
}

// List answers every state document by id.
func (s *MongoStateStore) List(ctx context.Context) (map[string]*StateRecord, error) {
	cursor, err := s.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("pricefeed: list states: %w", err)
	}
	out := map[string]*StateRecord{}
	for cursor.Next(ctx) {
		var doc stateDoc
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("pricefeed: list states: %w", err)
		}
		rec := doc.StateRecord
		out[doc.ID] = &rec
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("pricefeed: list states: %w", err)
	}
	return out, nil
}
