package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"math"
	"time"
)

// valueParent reads only the immutable selector and publication metadata.
// Payload decoding is deferred until this header has been validated.
func (s *Store) valueParent(ctx context.Context, id string) (*store.Run, error) {
	return s.readRunFormatParent(ctx, id)
}

func (s *Store) valueReadFormat(ctx context.Context, id string) (store.ValueFormat, error) {
	p, err := s.valueParent(ctx, id)
	if err != nil {
		return 0, err
	}
	if p == nil {
		p = &store.Run{ID: id, FormatVersion: 1}
	}
	raw, err := store.CheckRunFormatReplacement(ctx, p, p)
	if err != nil {
		return 0, err
	}
	return store.NormalizeValueFormat(raw)
}
func (s *Store) valueWriteFormat(ctx context.Context, id string) (store.ValueFormat, error) {
	p, err := s.valueParent(ctx, id)
	if err != nil {
		return 0, err
	}
	raw := 0
	if p != nil {
		raw = p.FormatVersion
	}
	return store.CheckRunValueFormat(ctx, id, raw)
}

// withValueRecordWrite serializes publication against birth/delete with a real
// parent write. The callback contains only Mongo operations and may be retried;
// external uploads and user codecs must already have completed.
func (s *Store) withValueRecordWrite(ctx context.Context, id string, apply func(context.Context, store.ValueFormat, *store.Run) error) error {
	original, err := store.ValueFormatForWrite(ctx, id)
	if err != nil {
		return err
	}
	sess, err := s.client.StartSession()
	if err != nil {
		return err
	}
	defer sess.EndSession(ctx)
	_, err = sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		p, err := s.valueParent(sc, id)
		if err != nil {
			return nil, err
		}
		raw := 0
		if p != nil {
			raw = p.FormatVersion
		}
		if _, err = store.CheckRunValueFormat(sc, id, raw); err != nil {
			return nil, err
		}
		if p == nil {
			// A legacy orphan reserves the same unique key that birth will insert.
			// Both operations are inside this transaction; no placeholder is durable.
			temporary := bson.M{"_id": id, "format_version": 1}
			if tenant, ok := store.TenantFromContext(sc); ok {
				temporary["tenant_id"] = tenant
			}
			if _, err = s.runs.InsertOne(sc, temporary); err != nil {
				return nil, err
			}
			if _, err = s.runs.DeleteOne(sc, withTenantFilter(sc, bson.M{"_id": id})); err != nil {
				return nil, err
			}
		} else {
			filter := notDeleted(withTenantFilter(sc, bson.M{"_id": id, "$and": bson.A{valueFormatPredicate(original), correctionVersionFilter(p.CASVersion)}}))
			changed, err := s.runs.UpdateOne(sc, filter, versionRunUpdate(bson.M{"$set": bson.M{"updated_at": time.Now().UTC()}}))
			if err != nil {
				return nil, err
			}
			if changed.MatchedCount != 1 {
				return nil, store.ErrRunConflict
			}
		}
		return nil, apply(sc, original, p)
	})
	if errors.Is(err, errValuePublicationNoop) {
		return nil
	}
	return err
}
func valueRecordDocument(ctx context.Context, f store.ValueFormat, kind store.ExactRecordKind, src any) (bson.M, error) {
	raw, err := store.EncodeValueRecordBSON(ctx, f, kind, src)
	if err != nil {
		return nil, err
	}
	var doc bson.M
	if err = bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, ctx.Err()
}
func decodeValueBSON(ctx context.Context, f store.ValueFormat, kind store.ExactRecordKind, raw bson.Raw, dst any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f == store.ExactValueFormat {
		codec, err := store.ExactRecordCodec(kind)
		if err != nil {
			return err
		}
		return codec.DecodeBSON(ctx, raw, dst)
	}
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.SetRegistry(Registry())
	if dest, ok := dst.(*store.Event); kind == store.ExactEvent && ok && dest != nil {
		var value store.Event
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		*dest = value
		return nil
	}
	return dec.Decode(dst)
}

// The counter can predate this process or have been independently cleaned.
// Seed within the same transaction before allocation, so a duplicate cannot
// retry forever on an increment rolled back by a failed child insertion.
func (s *Store) publishValueEvent(ctx context.Context, runID string, doc bson.M) (int64, error) {
	parent, e := s.valueParent(ctx, runID)
	if e != nil {
		return 0, e
	}
	n, e := s.streamNamespace(ctx, runID, parent)
	if e != nil {
		return 0, e
	}
	var last struct {
		Seq int64 `bson:"seq"`
	}
	err := n.events.FindOne(ctx, n.filter(ctx, nil), options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}}).SetProjection(bson.M{"seq": 1})).Decode(&last)
	floor := int64(0)
	if err == nil {
		if last.Seq == math.MaxInt64 {
			return 0, fmt.Errorf("store/mongo: event sequence exhausted")
		}
		floor = last.Seq + 1
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		return 0, err
	}
	if err = s.seedStreamSeqField(ctx, n, "next_seq", floor); err != nil {
		return 0, err
	}
	seq, err := s.allocStreamSeqField(ctx, n, "next_seq")
	if err != nil {
		return 0, err
	}
	copy := make(bson.M, len(doc)+1)
	for k, v := range doc {
		copy[k] = v
	}
	copy["seq"] = seq
	n.stamp(copy)
	if _, err = n.events.InsertOne(ctx, copy); err != nil {
		return 0, err
	}
	return seq, nil
}

var errValuePublicationNoop = errors.New("value publication unchanged")

func checkpointValueDocuments(ctx context.Context, f store.ValueFormat, cp *store.Checkpoint) (any, any, error) {
	if cp == nil {
		return nil, nil, ctx.Err()
	}
	whole, err := valueRecordDocument(ctx, f, store.ExactCheckpoint, cp)
	if err != nil {
		return nil, nil, err
	}
	if cp.InteractionID == "" && len(cp.InteractionQuestions) == 0 {
		return whole, whole, nil
	}
	copy := *cp
	copy.InteractionID = ""
	copy.InteractionQuestions = nil
	consumed, err := valueRecordDocument(ctx, f, store.ExactCheckpoint, &copy)
	return whole, consumed, err
}
