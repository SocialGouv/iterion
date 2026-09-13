package mongo

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

type portActivationDocument struct {
	ID                   string `bson:"_id"`
	store.PortActivation `bson:",inline"`
}

func (s *Store) portActivationCollection() *mongo.Collection {
	journal := true
	return s.db.Collection("activation_ports_v1", options.Collection().
		SetWriteConcern(&writeconcern.WriteConcern{W: "majority", Journal: &journal}).
		SetReadConcern(readconcern.Majority()))
}

func (s *Store) LoadPortActivation(ctx context.Context) (*store.PortActivation, error) {
	var doc portActivationDocument
	err := s.portActivationCollection().FindOne(ctx, bson.M{"_id": "current"}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := doc.PortActivation.Validate(); err != nil {
		return nil, err
	}
	return &doc.PortActivation, nil
}

func (s *Store) SavePortActivation(ctx context.Context, expectedRevision uint64, next *store.PortActivation) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.Revision != expectedRevision+1 {
		return fmt.Errorf("%w: activation revision", store.ErrRunConflict)
	}
	doc := portActivationDocument{ID: "current", PortActivation: *next}
	if expectedRevision == 0 {
		_, err := s.portActivationCollection().InsertOne(ctx, doc)
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: activation already exists", store.ErrRunConflict)
		}
		return err
	}
	result, err := s.portActivationCollection().ReplaceOne(ctx, bson.M{"_id": "current", "revision": expectedRevision}, doc)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("%w: activation revision changed", store.ErrRunConflict)
	}
	return nil
}
