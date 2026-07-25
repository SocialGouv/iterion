package storekit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// TicketMongo is the Mongo half of the single-use TTL ticket pair, for
// deployments where mint and redeem can land on different replicas. Docs
// are keyed by the ticket (_id) with an absolute expires_at reaped by a
// TTL index AND re-checked on Redeem (TTL deletion is lazy, ~60s);
// FindOneAndDelete gives atomic single-use. The payload's bson field
// name is caller-supplied so each domain keeps its existing wire format
// ("result" for desktopsso, "identity" for wsticket).
type TicketMongo[T any] struct {
	coll  *mongo.Collection
	field string
}

// NewTicketMongo wires a Mongo-backed ticket store storing payloads
// under payloadField.
func NewTicketMongo[T any](coll *mongo.Collection, payloadField string) *TicketMongo[T] {
	return &TicketMongo[T]{coll: coll, field: payloadField}
}

// EnsureSchema creates the TTL index on expires_at, idempotently.
func (s *TicketMongo[T]) EnsureSchema(ctx context.Context, errMsg string) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "expires_at", Value: 1}},
			Options: options.Index().SetName("ttl_expires_at").SetExpireAfterSeconds(0),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("%s: %w", errMsg, err)
	}
	return nil
}

// Mint stores v under a fresh random ticket expiring after ttl and
// returns the ticket.
func (s *TicketMongo[T]) Mint(ctx context.Context, v T, ttl time.Duration, errMsg string) (string, error) {
	ticket, err := NewTicket()
	if err != nil {
		return "", err
	}
	doc := bson.D{
		{Key: "_id", Value: ticket},
		{Key: s.field, Value: v},
		{Key: "expires_at", Value: time.Now().Add(ttl)},
	}
	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		return "", fmt.Errorf("%s: %w", errMsg, err)
	}
	return ticket, nil
}

// Redeem atomically removes and returns the payload stored under ticket,
// mapping unknown / expired tickets to notFound. An expired ticket is
// still consumed. The payload is decoded through a raw lookup on the
// configured field so T never needs storekit-owned bson tags.
func (s *TicketMongo[T]) Redeem(ctx context.Context, ticket string, notFound error, errMsg string) (T, error) {
	var zero T
	raw, err := s.coll.FindOneAndDelete(ctx, bson.M{"_id": ticket}).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return zero, notFound
	}
	if err != nil {
		return zero, fmt.Errorf("%s: %w", errMsg, err)
	}
	// A missing or non-datetime expires_at reads as expired — same
	// outcome as the zero-time decode of a struct-tagged doc.
	if exp, ok := raw.Lookup("expires_at").TimeOK(); !ok || time.Now().After(exp) {
		return zero, notFound
	}
	var out T
	if err := raw.Lookup(s.field).Unmarshal(&out); err != nil {
		return zero, fmt.Errorf("%s: %w", errMsg, err)
	}
	return out, nil
}
