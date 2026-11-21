package mongo

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// runBirthHeaderAliasPattern selects every key the canonical header parser
// folds to birth_id (JSON EqualFold and BSON ToLower, dotted-I included), the
// same family runDataHeaderProjection preserves. `\z` ends it, as every key
// pattern of this package (tenantHeaderAliasPattern says why).
const runBirthHeaderAliasPattern = `^[bB][iIİ][rR][tT][hH]_[iIİ][dD]\z`

func birthHeaderEntries() bson.M {
	return bson.M{"$filter": bson.M{
		"input": bson.M{"$objectToArray": "$$ROOT"}, "as": "header",
		"cond": bson.M{"$regexMatch": bson.M{"input": "$$header.k", "regex": runBirthHeaderAliasPattern}},
	}}
}

// canonicalBirthPredicate matches exactly the documents
// store.RunDataHeaderFromBSON reads as holding birth: no birth key at all
// for the historical empty birth, else one key spelled exactly birth_id
// whose value is that string. A duplicate, an alias spelling or a nonstring
// value matches nothing. It is evaluated by the mutating command itself.
func canonicalBirthPredicate(birth string) bson.M {
	entry := bson.M{"$arrayElemAt": bson.A{"$$headers", 0}}
	value := bson.M{"$getField": bson.M{"field": "v", "input": entry}}
	singleton := bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$getField": bson.M{"field": "k", "input": entry}}, "birth_id"}},
		bson.M{"$eq": bson.A{bson.M{"$type": value}, "string"}},
		bson.M{"$eq": bson.A{value, bson.M{"$literal": birth}}},
	}}
	return bson.M{"$expr": bson.M{"$let": bson.M{
		"vars": bson.M{"headers": birthHeaderEntries()},
		"in": bson.M{"$switch": bson.M{
			"branches": bson.A{
				bson.M{"case": bson.M{"$eq": bson.A{bson.M{"$size": "$$headers"}, 0}}, "then": birth == ""},
				bson.M{"case": bson.M{"$eq": bson.A{bson.M{"$size": "$$headers"}, 1}}, "then": singleton},
			}, "default": false,
		}},
	}}}
}

// mergeCAS appends the expected birth to a merge compare-and-set clause list.
func mergeCAS(cas bson.A, expect store.ExpectedBirth) bson.A {
	if expect.Check {
		cas = append(cas, canonicalBirthPredicate(expect.ID))
	}
	return cas
}

// checkMergeMissBirth classifies a merge compare-and-set that matched
// nothing: a run under id holding another birth than expected answers
// ErrRunDataIdentityChanged. An absent or deleted run answers nil, so the
// caller's own probe reports it as before.
func (s *Store) checkMergeMissBirth(ctx context.Context, id string, expect store.ExpectedBirth) error {
	if !expect.Check {
		return nil
	}
	h, err := s.ObserveRunDataHeader(ctx, id)
	if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store/mongo: merge CAS on run %s: read its birth: %w", id, err)
	}
	return expect.Verify(id, h.BirthID)
}
