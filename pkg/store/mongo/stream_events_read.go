package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (s *Store) readStreamEvents(ctx context.Context, n streamDataNamespace, extra bson.M, limit int) ([]*store.Event, error) {
	opts := options.Find().SetSort(bson.D{{Key: "seq", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cur, err := n.events.Find(ctx, n.filter(ctx, extra), opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []*store.Event{}
	for cur.Next(ctx) {
		var e store.Event
		if err := decodeValueBSON(ctx, n.format, store.ExactEvent, cur.Current, &e); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) loadStreamEvents(ctx context.Context, id string, extra bson.M, limit int) ([]*store.Event, error) {
	var out []*store.Event
	err := s.withStreamRead(ctx, id, func(sc context.Context, n streamDataNamespace, _ *store.Run) error {
		out = nil
		f, err := s.valueReadFormat(sc, id)
		if err != nil {
			return err
		}
		n.format = f
		out, err = s.readStreamEvents(sc, n, extra, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) LoadEvents(ctx context.Context, id string) ([]*store.Event, error) {
	return s.loadStreamEvents(ctx, id, nil, 0)
}
func (s *Store) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	seq := bson.M{"$gte": from}
	if to > 0 {
		seq["$lt"] = to
	}
	return s.loadStreamEvents(ctx, id, bson.M{"seq": seq}, limit)
}

// ScanEvents captures an inclusive initial tail, then commits short snapshot
// batches before invoking visitors. Appends cannot extend this scan indefinitely.
// This is a retained-birth scan, not a snapshot of all rows across every batch.
func (s *Store) ScanEvents(ctx context.Context, id string, visit func(*store.Event) bool) error {
	var ceiling int64
	var found bool
	var retained context.Context
	err := s.withStreamRead(ctx, id, func(sc context.Context, n streamDataNamespace, parent *store.Run) error {
		found = false
		ceiling = 0
		retained = nil
		f, err := s.valueReadFormat(sc, id)
		if err != nil {
			return err
		}
		h := store.RunDataHeader{RunID: id, Format: f}
		if parent != nil {
			h.Protocol = parent.DataProtocol
			h.BirthID = parent.BirthID
		}
		binding, err := store.CaptureRunDataBinding(h)
		if err != nil {
			return err
		}
		retained, err = store.WithRunDataBinding(ctx, binding)
		if err != nil {
			return err
		}
		var tail struct {
			Seq int64 `bson:"seq"`
		}
		err = n.events.FindOne(sc, n.filter(sc, nil), options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}})).Decode(&tail)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		if err != nil {
			return err
		}
		ceiling = tail.Seq
		found = true
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	var last int64
	var delivered bool
	for {
		var batch []*store.Event
		err = s.withStreamRead(retained, id, func(sc context.Context, n streamDataNamespace, _ *store.Run) error {
			batch = nil
			f, err := s.valueReadFormat(sc, id)
			if err != nil {
				return err
			}
			n.format = f
			seq := bson.M{"$lte": ceiling}
			if delivered {
				seq["$gt"] = last
			}
			batch, err = s.readStreamEvents(sc, n, bson.M{"seq": seq}, 128)
			return err
		})
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, e := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !visit(e) {
				return nil
			}
			last = e.Seq
			delivered = true
		}
	}
}
