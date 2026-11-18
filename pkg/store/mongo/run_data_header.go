package mongo

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// All original key spellings survive projection. The raw header parser, not
// projection's output spelling, decides whether a selector is canonical.
func runDataHeaderProjection() bson.M {
	aliases := bson.A{
		bson.M{"$regexMatch": bson.M{"input": "$$header.k", "regex": runFormatHeaderAliasPattern}},
		bson.M{"$regexMatch": bson.M{"input": "$$header.k", "regex": "^(?:_[iIİ][dD]|[bB][iIİ][rR][tT][hH]_[iIİ][dD]|[dD][aA][tT][aA]_[pP][rR][oO][tT][oO][cC][oO][lL]|[vV]|[dD][eE][lL][eE][tT][eE][dD]_[aA][tT])$"}},
	}
	return bson.M{"_id": 0, "data_headers": bson.M{"$filter": bson.M{
		"input": bson.M{"$objectToArray": "$$ROOT"}, "as": "header", "cond": bson.M{"$or": aliases},
	}}}
}

// ObserveRunDataHeader never decodes Inputs, correction state or other payload
// fields. Header observation does not enable incarnation-scoped data storage.
// A binding that observed a tenant is compared with the canonical tenant of
// the same document after its header.
func (s *Store) ObserveRunDataHeader(ctx context.Context, id string) (store.RunDataHeader, error) {
	if ctx == nil {
		return store.RunDataHeader{}, store.ErrInvalidRunDataIdentity
	}
	if err := ctx.Err(); err != nil {
		return store.RunDataHeader{}, err
	}
	if id == "" {
		return store.RunDataHeader{}, store.ErrInvalidRunDataIdentity
	}
	observed := observesTenant(ctx)
	projection := runDataHeaderProjection()
	if observed {
		projection["retained_tenant_headers"] = tenantHeaderEntries()
	}
	cursor, err := s.runs.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: withTenantFilter(ctx, bson.M{"_id": id})}},
		{{Key: "$project", Value: projection}},
	})
	if err != nil {
		return store.RunDataHeader{}, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return store.RunDataHeader{}, err
		}
		return store.RunDataHeader{}, store.ErrRunNotFound
	}
	raw := cursor.Current
	array, ok := raw.Lookup("data_headers").ArrayOK()
	if !ok {
		return store.RunDataHeader{}, store.ErrInvalidRunDataIdentity
	}
	entries, err := array.Values()
	if err != nil {
		return store.RunDataHeader{}, err
	}
	headers := bson.D{}
	seenMetadata := map[string]bool{}
	var schemaVersion int64
	deleted := false
	for _, entry := range entries {
		document, ok := entry.DocumentOK()
		if !ok {
			return store.RunDataHeader{}, store.ErrInvalidRunDataIdentity
		}
		key, ok := document.Lookup("k").StringValueOK()
		if !ok {
			return store.RunDataHeader{}, store.ErrInvalidRunDataIdentity
		}
		value := document.Lookup("v")
		canonical := strings.ToLower(key)
		switch canonical {
		case "v", "deleted_at":
			if key != canonical || seenMetadata[canonical] {
				return store.RunDataHeader{}, fmt.Errorf("%w: duplicate or noncanonical %s", store.ErrInvalidRunDataIdentity, canonical)
			}
			seenMetadata[canonical] = true
			if canonical == "v" {
				switch value.Type {
				case bson.TypeInt32:
					schemaVersion = int64(value.Int32())
				case bson.TypeInt64:
					schemaVersion = value.Int64()
				default:
					return store.RunDataHeader{}, fmt.Errorf("%w: noninteger schema version", store.ErrInvalidRunDataIdentity)
				}
			} else {
				deleted = value.Type != bson.TypeNull
			}
		default:
			headers = append(headers, bson.E{Key: key, Value: value})
		}
	}
	encoded, err := bson.Marshal(headers)
	if err != nil {
		return store.RunDataHeader{}, err
	}
	h, err := store.RunDataHeaderFromBSON(ctx, encoded)
	if err != nil {
		return store.RunDataHeader{}, err
	}
	h, err = store.ValidateRunDataObservation(ctx, id, h)
	if err != nil {
		return store.RunDataHeader{}, err
	}
	if observed {
		tenant, err := retainedCanonicalTenant(raw)
		if err != nil {
			return store.RunDataHeader{}, err
		}
		if err := store.CheckRunObservedTenant(ctx, tenant); err != nil {
			return store.RunDataHeader{}, err
		}
	}
	if schemaVersion > SchemaVersion {
		return store.RunDataHeader{}, fmt.Errorf("store/mongo: run %s schema version %d unknown, upgrade required: %w", id, schemaVersion, store.ErrRunMetadataUndecodable)
	}
	if deleted {
		return store.RunDataHeader{}, store.ErrRunDeleted
	}
	return h, nil
}

var _ store.RunDataHeaderStore = (*Store)(nil)
