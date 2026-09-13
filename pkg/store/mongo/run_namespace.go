package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const NativeSchemaVersion = 2

// Mongo's generic interface decoding has different numeric representations
// from JSON. Native public input values retain their exact JSON payload in
// the same atomic run document; inputs is absent on that persisted shape.
// This also distinguishes missing, null, an empty array and a large integer.
type storedRunDocument struct {
	store.Run    `bson:",inline"`
	PublicInputs []byte `bson:"ports_inputs_json,omitempty"`
}

func runInsertDocument(r *store.Run) (any, error) {
	if !store.IsNativeRunID(r.ID) {
		return r, nil
	}
	data, err := json.Marshal(r.Inputs)
	if err != nil {
		return nil, err
	}
	copy := *r
	copy.Inputs = nil
	return storedRunDocument{Run: copy, PublicInputs: data}, nil
}

func (d *storedRunDocument) decodePublicInputs() error {
	if !store.IsNativeRunID(d.ID) {
		return nil
	}
	if len(d.PublicInputs) == 0 || !json.Valid(d.PublicInputs) {
		return fmt.Errorf("store/mongo: native run %s has no valid public input payload: %w", d.ID, store.ErrRunSemantics)
	}
	decoder := json.NewDecoder(bytes.NewReader(d.PublicInputs))
	decoder.UseNumber()
	return decoder.Decode(&d.Inputs)
}

func (s *Store) guardNativeRun(ctx context.Context, id string) error {
	if err := store.ValidateRunID(id); err != nil {
		return err
	}
	if store.IsNativeRunID(id) {
		_, err := s.LoadRun(ctx, id)
		return err
	}
	return nil
}

// collectionForRun routes within this Store and database. No lookup or
// existence-based fallback can redirect a native ID to a legacy collection.
func (s *Store) collectionForRun(id string, legacy *mongo.Collection) *mongo.Collection {
	if store.IsNativeRunID(id) {
		return s.db.Collection(legacy.Name() + "_ports_v1")
	}
	if store.RunDataDirectory(id) != "runs" {
		return s.db.Collection(legacy.Name() + "_unsupported_ports")
	}
	return legacy
}

func schemaVersionForRun(id string) int {
	if store.IsNativeRunID(id) {
		return NativeSchemaVersion
	}
	return SchemaVersion
}

// namespaceFilter excludes old shadow documents carrying reserved IDs. The
// native half must carry its supported semantic and format identity too.
func namespaceFilter(filter any, native bool) bson.M {
	identity := bson.M{"_id": bson.M{"$not": bson.M{"$regex": `^pc[0-9]+_`}}}
	if native {
		identity = bson.M{"_id": bson.M{"$regex": `^pc1_.+`},
			"format_version":    store.NativeRunFormatVersion,
			"v":                 NativeSchemaVersion,
			"runtime_semantics": bson.M{"$in": bson.A{store.RuntimeSemanticsPortsV1, store.RuntimeSemanticsLegacyAdapterV1}},
		}
	}
	return bson.M{"$and": bson.A{filter, identity}}
}

// findAllRuns merges both authoritative namespaces before applying sort and
// limit. Applying a limit independently to each family would change list,
// retention and orphan-recovery semantics or starve one family.
func (s *Store) findAllRuns(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*mongo.Cursor, error) {
	var config options.FindOptions
	for _, opt := range opts {
		for _, apply := range opt.List() {
			if err := apply(&config); err != nil {
				return nil, err
			}
		}
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: namespaceFilter(filter, false)}},
		{{Key: "$unionWith", Value: bson.M{"coll": colRuns + "_ports_v1", "pipeline": mongo.Pipeline{
			{{Key: "$match", Value: namespaceFilter(filter, true)}},
		}}}},
	}
	if config.Sort != nil {
		pipeline = append(pipeline, bson.D{{Key: "$sort", Value: config.Sort}})
	}
	if config.Skip != nil && *config.Skip > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$skip", Value: *config.Skip}})
	}
	if config.Limit != nil && *config.Limit != 0 {
		if *config.Limit < 0 {
			return nil, fmt.Errorf("store/mongo: negative combined run limit")
		}
		pipeline = append(pipeline, bson.D{{Key: "$limit", Value: *config.Limit}})
	}
	if config.Projection != nil {
		pipeline = append(pipeline, bson.D{{Key: "$project", Value: config.Projection}})
	}
	return s.runs.Aggregate(ctx, pipeline)
}

func (s *Store) countAllRuns(ctx context.Context, filter any) (int64, error) {
	var total int64
	for _, native := range []bool{false, true} {
		coll := s.runs
		if native {
			coll = s.collectionForRun(store.NativeRunIDPrefix+"namespace", coll)
		}
		n, err := coll.CountDocuments(ctx, namespaceFilter(filter, native))
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
