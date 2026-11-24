package mongo

import (
	"context"
	"fmt"
	"sort"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

// Derive the projection from the already encoded record, exactly as readers
// will materialize it. No caller value or custom codec is evaluated twice.
// Explicit null clears an obsolete projection through unknown-field merging.
func setInputPRURLProjection(ctx context.Context, doc bson.M, raw store.ValueFormat) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	format, err := store.NormalizeValueFormat(int(raw))
	if err != nil {
		return err
	}
	var value any
	if format == store.ExactValueFormat {
		inputs, err := valuecodec.DecodeMap(ctx, doc["inputs"])
		if err != nil {
			return err
		}
		value = inputs["pr_url"]
	} else {
		switch inputs := doc["inputs"].(type) {
		case bson.D:
			for _, field := range inputs {
				if field.Key == "pr_url" {
					value = field.Value
				}
			}
		case bson.M:
			value = inputs["pr_url"]
		case map[string]any:
			value = inputs["pr_url"]
		}
	}
	var projected any
	if text, ok := value.(string); ok {
		projected = text
	}
	doc["inputs_pr_url"] = projected
	return ctx.Err()
}

// ListRunsForPR reads the legacy and exact indexes in one snapshot, unions
// identities, and orders by created_at then ID. A matching unknown header is
// refused, never silently filtered; this metadata view does not certify other
// runs of the tenant or decode their payloads. Any transaction error discards
// all IDs, including an error after the callback produced its result.
func (s *Store) ListRunsForPR(ctx context.Context, prURL string) ([]string, error) {
	if prURL == "" {
		return []string{}, nil
	}
	if ctx == nil {
		return nil, store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := s.client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	var ids []string
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		ids = nil
		seen := map[string]*store.Run{}
		for _, field := range []string{"inputs.pr_url", "inputs_pr_url"} {
			projection := runFormatMetadataProjection()
			projection["created_at"] = 1
			projection["value_pr_kind"] = bson.M{"$type": "$" + field}
			cur, err := s.runs.Aggregate(sc, mongo.Pipeline{
				{{Key: "$match", Value: notDeleted(withTenantFilter(sc, bson.M{field: prURL}))}},
				{{Key: "$project", Value: projection}},
			})
			if err != nil {
				return nil, err
			}
			for cur.Next(sc) {
				row, e := decodeRunFormatMetadata(sc, cur.Current)
				if e != nil {
					_ = cur.Close(sc)
					return nil, e
				}
				// Mongo equality also matches array elements. Only a scalar string
				// equals the public Inputs["pr_url"] value. Validate the header first:
				// an indexed future-format candidate must still refuse the whole view.
				if cur.Current.Lookup("value_pr_kind").StringValue() != "string" {
					continue
				}
				seen[row.ID] = row
			}
			if err = cur.Err(); err != nil {
				_ = cur.Close(sc)
				return nil, err
			}
			if err = cur.Close(sc); err != nil {
				return nil, err
			}
		}
		rows := make([]*store.Run, 0, len(seen))
		for _, row := range seen {
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
				return rows[i].ID < rows[j].ID
			}
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		})
		ids = make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		return nil, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()))
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list runs for pr: %w", err)
	}
	return ids, nil
}
