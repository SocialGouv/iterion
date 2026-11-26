package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

var _ store.RunWindowListingStore = (*Store)(nil)

// runWindowSort is the listing order: newest first, the id breaking ties so
// the order is total and a keyset page boundary is reproducible. The
// created_id_desc index (EnsureSchema) hands it back without a sort stage.
var runWindowSort = bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}

// windowBound is a keyset position in runWindowSort. createdAt and id are
// either Go values (a caller's RunKey) or the raw BSON values of the last
// document a previous read returned; inclusive drops the id arm and keeps
// every run at or before createdAt.
type windowBound struct {
	createdAt any
	id        any
	inclusive bool
}

// ListRunWindow answers a listing window with one indexed Find
// (store.RunWindowListingStore). Records are decoded by decodeRunValueRecord —
// the very decode LoadRun runs — and refused by listingRecord, so a document
// LoadRunForListing would refuse comes back as a SkippedRun, never as a record
// and never as a failure of the page.
//
// A skipped document still consumed a slot of the read's limit, so when the
// read came back full but short of w.Limit records the window continues from
// the last document it saw, until it is filled or the collection is exhausted.
func (s *Store) ListRunWindow(ctx context.Context, w store.RunWindow) ([]*store.Run, []store.SkippedRun, error) {
	if w.Limit < 0 {
		return nil, nil, fmt.Errorf("store/mongo: list run window: negative limit %d", w.Limit)
	}
	out := []*store.Run{}
	var skipped []store.SkippedRun
	bound := keyBound(w.Before)
	for {
		want := 0
		if w.Limit > 0 {
			want = w.Limit - len(out)
		}
		filter, opts := runWindowQuery(ctx, w, bound, want)
		seen, last, err := s.readRunWindow(ctx, filter, opts, &out, &skipped)
		if err != nil {
			return nil, nil, err
		}
		if want == 0 || seen < want || len(out) >= w.Limit {
			return out, skipped, nil
		}
		if last == nil {
			return out, skipped, nil
		}
		bound = last
	}
}

// readRunWindow drains one Find into out / skipped and reports how many
// documents it saw and the keyset position of the last one.
func (s *Store) readRunWindow(ctx context.Context, filter bson.M, opts *options.FindOptionsBuilder, out *[]*store.Run, skipped *[]store.SkippedRun) (int, *windowBound, error) {
	cur, err := s.runs.Find(ctx, filter, opts)
	if err != nil {
		return 0, nil, fmt.Errorf("store/mongo: list run window: %w", err)
	}
	defer cur.Close(ctx)
	seen := 0
	var last *windowBound
	for cur.Next(ctx) {
		seen++
		raw := cur.Current
		idValue := cloneRawValue(raw.Lookup("_id"))
		createdValue, hasCreated := raw.Lookup("created_at"), true
		if createdValue.Type == 0 {
			hasCreated = false
		}
		if hasCreated {
			last = &windowBound{createdAt: cloneRawValue(createdValue), id: idValue}
		} else {
			// Nothing sorts below a missing created_at in descending order and
			// no bound can name it: the read that returned it reached the end.
			last = nil
		}
		id, ok := idValue.StringValueOK()
		if !ok {
			*skipped = append(*skipped, store.SkippedRun{ID: idValue.String(), Err: store.RunMetadataUndecodable(idValue.String(), fmt.Errorf("run _id is BSON %s, not a string", idValue.Type))})
			continue
		}
		r, err := decodeRunValueRecord(ctx, id, raw, cur.Decode)
		if err != nil {
			*skipped = append(*skipped, store.SkippedRun{ID: id, Err: err})
			continue
		}
		rec, err := listingRecord(id, r)
		if err != nil {
			*skipped = append(*skipped, store.SkippedRun{ID: id, Err: err})
			continue
		}
		*out = append(*out, rec)
	}
	if err := cur.Err(); err != nil {
		return 0, nil, fmt.Errorf("store/mongo: list run window: %w", err)
	}
	return seen, last, nil
}

// cloneRawValue detaches a value from the cursor's buffer, which the next
// document overwrites.
func cloneRawValue(v bson.RawValue) bson.RawValue {
	v.Value = append([]byte(nil), v.Value...)
	return v
}

// keyBound turns a caller's RunKey into a bound. A zero CreatedAt is no bound.
//
// Mongo stores a date to the millisecond. A key whose CreatedAt carries a
// sub-millisecond part sits strictly between two storable instants, so every
// run at or before its millisecond floor is older than it, whatever its id —
// sent as is, the driver would truncate it and the id arm would wrongly drop
// the runs of that floor.
func keyBound(k store.RunKey) *windowBound {
	if k.CreatedAt.IsZero() {
		return nil
	}
	floor := k.CreatedAt.Truncate(time.Millisecond)
	if !floor.Equal(k.CreatedAt) {
		return &windowBound{createdAt: floor, inclusive: true}
	}
	return &windowBound{createdAt: k.CreatedAt, id: k.ID}
}

// ceilMillisecond rounds t up to the next storable instant, so that
// `updated_at >= ceil(t)` over millisecond dates answers exactly what
// `!UpdatedAt.Before(t)` answers in Go.
func ceilMillisecond(t time.Time) time.Time {
	floor := t.Truncate(time.Millisecond)
	if floor.Equal(t) {
		return t
	}
	return floor.Add(time.Millisecond)
}

// runWindowQuery is the exact Find ListRunWindow sends for w from bound,
// capped at limit documents (0 = none). Exposed to the plan tests, which
// explain this very query.
func runWindowQuery(ctx context.Context, w store.RunWindow, bound *windowBound, limit int) (bson.M, *options.FindOptionsBuilder) {
	filter := bson.M{}
	var and bson.A
	if w.Status != "" {
		filter["status"] = w.Status
	}
	if w.Workflow != "" {
		filter["workflow_name"] = w.Workflow
	}
	if !w.Since.IsZero() {
		filter["updated_at"] = bson.M{"$gte": ceilMillisecond(w.Since)}
	}
	if len(w.TenantIDs) > 0 {
		// In an $and, never at the top level: withTenantFilter sets the
		// top-level tenant_id to the ctx's tenant and would overwrite the set.
		in := bson.A{}
		for _, id := range w.TenantIDs {
			if id == "" {
				// A run with no tenant stores no tenant_id (omitempty).
				in = append(in, nil, "")
				continue
			}
			in = append(in, id)
		}
		and = append(and, bson.M{"tenant_id": bson.M{"$in": in}})
	}
	if bound != nil {
		if bound.inclusive {
			and = append(and, bson.M{"created_at": bson.M{"$lte": bound.createdAt}})
		} else {
			// The range bounds the index scan; the $or only drops the runs
			// of the bound's own instant at or after its id.
			and = append(and,
				bson.M{"created_at": bson.M{"$lte": bound.createdAt}},
				bson.M{"$or": bson.A{
					bson.M{"created_at": bson.M{"$lt": bound.createdAt}},
					bson.M{"_id": bson.M{"$lt": bound.id}},
				}},
			)
		}
	}
	if len(and) > 0 {
		filter["$and"] = and
	}
	opts := options.Find().SetProjection(listingProjection()).SetSort(runWindowSort)
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	return notDeleted(withTenantFilter(ctx, filter)), opts
}
