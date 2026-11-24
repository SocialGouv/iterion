package mongo

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

// StreamSelection is an immutable original observation for a subscription.
// Historical empty birth IDs retain their original writer-version limitation.
// It owns no client, cursor or long-lived transaction.
type StreamSelection struct {
	s             *Store
	ns            streamDataNamespace
	present       bool
	observation   store.RunObservation
	binding       store.RunDataBinding
	bound         bool
	tenant, owner string
	privileged    bool
	format        store.ValueFormat
}

func (s *Store) ResolveStreamSelection(ctx context.Context, id string) (*StreamSelection, error) {
	if ctx == nil {
		return nil, store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := store.TenantFromContext(ctx); !ok && !store.IsWithoutTenantFilter(ctx) {
		return nil, store.ErrRunDataIdentityChanged
	}
	if _, err := store.ValueFormatForWrite(ctx, id); err != nil {
		return nil, err
	}
	session, err := s.client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	var out *StreamSelection
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		out = nil
		parent, _, err := s.readRunFormatObservationPolicy(sc, id, true, true)
		if err != nil {
			return nil, err
		}
		if parent != nil && parent.DeletedAt != nil {
			return nil, store.ErrRunDeleted
		}
		var binding store.RunDataBinding
		var bound bool
		if parent != nil {
			format, e := store.NormalizeValueFormat(parent.FormatVersion)
			if e != nil {
				return nil, e
			}
			binding, e = store.CaptureRunDataBinding(store.RunDataHeader{RunID: id, Format: format, Protocol: parent.DataProtocol, BirthID: parent.BirthID})
			if e != nil {
				return nil, e
			}
			sc, e = store.WithRunDataBinding(sc, binding)
			if e != nil {
				return nil, e
			}
			bound = true
		} else {
			binding, bound = store.RunDataBindingFrom(sc)
		}
		n, err := s.streamNamespace(sc, id, parent)
		if err != nil {
			return nil, err
		}
		sel := &StreamSelection{s: s, ns: n, present: parent != nil, format: n.format, privileged: store.IsWithoutTenantFilter(ctx), binding: binding, bound: bound}
		sel.tenant, _ = store.TenantFromContext(ctx)
		sel.owner, _ = store.OwnerFromContext(ctx)
		if parent != nil {
			sel.tenant = parent.TenantID
		}
		h := store.RunDataHeader{RunID: id, Format: n.format}
		if parent != nil {
			h.Protocol = parent.DataProtocol
			h.BirthID = parent.BirthID
		}
		sel.observation, err = store.CaptureRunObservation(h, sel.tenant, parent != nil)
		if err != nil {
			return nil, err
		}
		out = sel
		return nil, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (n *StreamSelection) operation(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return nil, store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := store.ValueFormatForWrite(ctx, n.ns.runID)
	if err != nil {
		return nil, err
	}
	if err = store.RequireRunValueFormat(ctx, n.ns.runID); err == nil && f != n.format {
		return nil, store.ErrValueFormatChanged
	} else if err != nil && !errors.Is(err, store.ErrValueFormatCapability) {
		return nil, err
	}
	if tenant, ok := store.TenantFromContext(ctx); ok && tenant != n.tenant {
		return nil, store.ErrRunDataIdentityChanged
	}
	if owner, ok := store.OwnerFromContext(ctx); ok && n.owner != "" && owner != n.owner {
		return nil, store.ErrRunDataIdentityChanged
	}
	ctx = store.WithIdentity(ctx, n.tenant, n.owner)
	if n.tenant == "" {
		if !n.privileged {
			return nil, store.ErrRunDataIdentityChanged
		}
		ctx = store.WithoutTenantFilter(ctx)
	}
	ctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: n.ns.runID, Format: n.format})
	if err != nil {
		return nil, err
	}
	if n.bound {
		return store.WithRunDataBinding(ctx, n.binding)
	}
	if _, ok := store.RunDataBindingFrom(ctx); ok {
		return nil, store.ErrRunDataIdentityChanged
	}
	return ctx, nil
}
func (n *StreamSelection) check(p *store.Run, selected streamDataNamespace) error {
	if (p != nil) != n.present {
		return store.ErrRunDataIdentityChanged
	}
	if p != nil && p.DeletedAt != nil {
		return store.ErrRunDeleted
	}
	if selected.namespace != n.ns.namespace || selected.format != n.format {
		return store.ErrRunDataIdentityChanged
	}
	if p != nil && p.TenantID != n.tenant {
		return store.ErrRunDataIdentityChanged
	}
	return nil
}
func (n *StreamSelection) read(ctx context.Context, apply func(context.Context, *store.Run) error) error {
	c, err := n.operation(ctx)
	if err != nil {
		return err
	}
	return n.s.withSelectedStreamReadPolicy(c, n.ns.runID, true, true, func(sc context.Context, selected streamDataNamespace, p *store.Run) error {
		if err := n.check(p, selected); err != nil {
			return err
		}
		return apply(sc, p)
	})
}
func (n *StreamSelection) Validate(ctx context.Context) error {
	return n.read(ctx, func(context.Context, *store.Run) error { return nil })
}
func (n *StreamSelection) Terminal(ctx context.Context) (bool, error) {
	var terminal bool
	err := n.read(ctx, func(_ context.Context, p *store.Run) error { terminal = p != nil && p.Status.IsTerminal(); return nil })
	if err != nil {
		return false, err
	}
	return terminal, nil
}
func (n *StreamSelection) EventsCollection() *mongo.Collection { return n.ns.events }
func (n *StreamSelection) LogsCollection() *mongo.Collection   { return n.ns.logs }
func (n *StreamSelection) filter() bson.D {
	f := bson.D{{Key: "run_id", Value: n.ns.runID}}
	if n.tenant != "" {
		f = append(f, bson.E{Key: "tenant_id", Value: n.tenant})
	}
	if n.ns.namespace != "" {
		f = append(f, bson.E{Key: "data_namespace", Value: n.ns.namespace})
	}
	return f
}
func (n *StreamSelection) EventFilter() bson.D { return n.filter() }
func (n *StreamSelection) LogFilter() bson.D   { return n.filter() }
func (n *StreamSelection) DecodeEvent(ctx context.Context, raw bson.Raw) (*store.Event, error) {
	c, err := n.operation(ctx)
	if err != nil {
		return nil, err
	}
	var e store.Event
	if err = decodeValueBSON(c, n.format, store.ExactEvent, raw, &e); err != nil {
		if streamContextErrorOnly(err) {
			return nil, err
		}
		return nil, errors.Join(store.ErrEventsCorrupted, err)
	}
	if e.RunID != n.ns.runID || (n.tenant != "" && e.TenantID != n.tenant) {
		return nil, store.ErrRunDataIdentityChanged
	}
	stamped, err := store.WithEventObservation(e, n.observation)
	if err != nil {
		return nil, err
	}
	return &stamped, nil
}

// BackfillEvents snapshots an initial finite tail, then commits short pages
// before delivering each batch. The cursor is inclusive unless after is true.
func (n *StreamSelection) BackfillEvents(ctx context.Context, from int64, after bool, pageSize int, emit func([]*store.Event) bool) error {
	if pageSize <= 0 {
		return fmt.Errorf("stream event page size must be positive")
	}
	var ceiling int64
	var found bool
	err := n.read(ctx, func(sc context.Context, _ *store.Run) error {
		found = false
		ceiling = 0
		if _, err := n.s.valueReadFormat(sc, n.ns.runID); err != nil {
			return err
		}
		var tail struct {
			Seq int64 `bson:"seq"`
		}
		err := decodeStreamOne(n.ns.events.FindOne(sc, n.filter(), options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}})), &tail)
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
	if err != nil || !found {
		return err
	}
	for {
		var rows []*store.Event
		err = n.read(ctx, func(sc context.Context, _ *store.Run) error {
			rows = nil
			seq := bson.M{"$lte": ceiling}
			if after {
				seq["$gt"] = from
			} else {
				seq["$gte"] = from
			}
			cur, err := n.ns.events.Find(sc, bson.D{{Key: "$and", Value: bson.A{n.filter(), bson.M{"seq": seq}}}}, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetLimit(int64(pageSize)))
			if err != nil {
				return err
			}
			defer cur.Close(sc)
			for cur.Next(sc) {
				e, err := n.DecodeEvent(sc, cur.Current)
				if err != nil {
					return err
				}
				rows = append(rows, e)
			}
			return cur.Err()
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !emit(rows) {
			return ctx.Err()
		}
		from = rows[len(rows)-1].Seq
		after = true
	}
}

func streamLogEnd(offset int64, data []byte) (int64, error) {
	if offset < 0 || int64(len(data)) > math.MaxInt64-offset {
		return 0, fmt.Errorf("%w: invalid raw log extent", store.ErrEventsCorrupted)
	}
	return offset + int64(len(data)), nil
}

// BackfillLogs keeps the anchor and page query in the same snapshot and never
// holds a transaction across the caller's delivery callback.
func (n *StreamSelection) BackfillLogs(ctx context.Context, from int64, emit func(int64, []byte) bool) error {
	if from < 0 {
		from = 0
	}
	var ceiling int64
	var found bool
	err := n.read(ctx, func(sc context.Context, _ *store.Run) error {
		found = false
		ceiling = 0
		var tail runLogDoc
		err := decodeStreamOne(n.ns.logs.FindOne(sc, n.filter(), options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}})), &tail)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		if err != nil {
			return err
		}
		ceiling, err = streamLogEnd(tail.Offset, tail.Data)
		found = err == nil
		return err
	})
	if err != nil || !found {
		return err
	}
	var lastOffset int64
	var delivered bool
	for {
		var rows []runLogDoc
		err = n.read(ctx, func(sc context.Context, _ *store.Run) error {
			rows = nil
			bounds := bson.M{"$lt": ceiling}
			if delivered {
				bounds["$gt"] = lastOffset
			} else if from > 0 {
				var anchor struct {
					Offset int64 `bson:"offset"`
				}
				e := decodeStreamOne(n.ns.logs.FindOne(sc, bson.D{{Key: "$and", Value: bson.A{n.filter(), bson.M{"offset": bson.M{"$lte": from}}}}}, options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}})), &anchor)
				if e == nil {
					bounds["$gte"] = anchor.Offset
				} else if !errors.Is(e, mongo.ErrNoDocuments) {
					return e
				}
			}
			cur, e := n.ns.logs.Find(sc, bson.D{{Key: "$and", Value: bson.A{n.filter(), bson.M{"offset": bounds}}}}, options.Find().SetSort(bson.D{{Key: "offset", Value: 1}}).SetLimit(128))
			if e != nil {
				return e
			}
			defer cur.Close(sc)
			for cur.Next(sc) {
				var doc runLogDoc
				if e = cur.Decode(&doc); e != nil {
					return errors.Join(store.ErrEventsCorrupted, e)
				}
				if _, e = streamLogEnd(doc.Offset, doc.Data); e != nil {
					return e
				}
				rows = append(rows, doc)
			}
			return cur.Err()
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if err = ctx.Err(); err != nil {
				return err
			}
			if !emit(row.Offset, row.Data) {
				return ctx.Err()
			}
			lastOffset = row.Offset
			delivered = true
		}
	}
}

// Preserve a real cancellation, but never let a joined cancellation erase a
// deterministic decoder error from the same operation.
func streamContextErrorOnly(err error) bool {
	if err == nil {
		return false
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		parts := many.Unwrap()
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !streamContextErrorOnly(part) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return streamContextErrorOnly(one.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

// Query failures (including cancellation, absence and retryable server errors)
// keep their original identity. Only decoding a received record is corruption.
func decodeStreamOne(result *mongo.SingleResult, dst any) error {
	raw, err := result.Raw()
	if err != nil {
		return err
	}
	if err = bson.Unmarshal(raw, dst); err != nil {
		return errors.Join(store.ErrEventsCorrupted, err)
	}
	return nil
}
