package mongo

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

const (
	colIncarnationEvents = "run_data_v1_events"
	colIncarnationLogs   = "run_data_v1_logs"
	colIncarnationSeq    = "run_data_v1_seq"
	colIncarnationPlans  = "run_data_v1_plans"
	colIncarnationNotes  = "run_data_v1_notes"
)

type streamDataNamespace struct {
	events, logs, seq, plans, notes *mongo.Collection
	runID, namespace, tenant        string
	format                          store.ValueFormat
}

func (s *Store) streamNamespace(ctx context.Context, id string, parent *store.Run) (streamDataNamespace, error) {
	f, err := store.ValueFormatForWrite(ctx, id)
	if err != nil {
		return streamDataNamespace{}, err
	}
	n := streamDataNamespace{events: s.events, logs: s.runLogs, seq: s.runSeq, plans: s.runPlans, notes: s.runNotes, runID: id, format: f}
	if parent == nil {
		if err = store.CheckMissingRunDataParent(ctx, id); err != nil {
			return n, err
		}
		return n, nil
	}
	if parent.ID != id {
		return n, store.ErrRunDataIdentityChanged
	}
	f, err = store.NormalizeValueFormat(parent.FormatVersion)
	if err != nil {
		return n, err
	}
	if _, err = store.CheckRunFormatReplacement(ctx, parent, parent); err != nil {
		return n, err
	}
	h := store.RunDataHeader{RunID: id, Format: f, Protocol: parent.DataProtocol, BirthID: parent.BirthID}
	if err = store.RequireRunDataStorage(h.Protocol); err != nil {
		return n, err
	}
	if err = store.CheckRunDataBinding(ctx, h); err != nil {
		return n, err
	}
	n.format = f
	if h.Protocol == store.HistoricalRunData {
		return n, nil
	}
	scope, err := store.ResolveIncarnationDataScope(ctx, h)
	if err != nil {
		return n, err
	}
	n.namespace, err = scope.BlobPrefix()
	if err != nil {
		return n, err
	}
	n.tenant = parent.TenantID
	n.events = s.db.Collection(colIncarnationEvents)
	n.logs = s.db.Collection(colIncarnationLogs)
	n.seq = s.db.Collection(colIncarnationSeq)
	n.plans = s.db.Collection(colIncarnationPlans)
	n.notes = s.db.Collection(colIncarnationNotes)
	return n, nil
}
func (n streamDataNamespace) filter(ctx context.Context, extra bson.M) bson.M {
	out := bson.M{"run_id": n.runID}
	for k, v := range extra {
		out[k] = v
	}
	if n.namespace != "" {
		out["data_namespace"] = n.namespace
		out["tenant_id"] = n.tenant
	}
	return withTenantFilter(ctx, out)
}
func (n streamDataNamespace) stamp(doc bson.M) {
	if n.namespace != "" {
		doc["data_namespace"] = n.namespace
		doc["tenant_id"] = n.tenant
	}
	doc["run_id"] = n.runID
}
func (n streamDataNamespace) counterKey(ctx context.Context) bson.D {
	if n.namespace != "" {
		return bson.D{{Key: "tenant_id", Value: n.tenant}, {Key: "data_namespace", Value: n.namespace}}
	}
	tenant, _ := store.TenantFromContext(ctx)
	return bson.D{{Key: "tenant_id", Value: tenant}, {Key: "run_id", Value: n.runID}}
}

// Every retry resets caller-owned temporary results in apply. No user callbacks
// run here. Header, anchor, and child reads share one snapshot.
func (s *Store) withStreamRead(ctx context.Context, id string, apply func(context.Context, streamDataNamespace, *store.Run) error) error {
	if ctx == nil {
		return store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.withSelectedStreamRead(ctx, id, false, apply)
}
func (s *Store) withOpaqueStreamRead(ctx context.Context, id string, apply func(context.Context, streamDataNamespace, *store.Run) error) error {
	return s.withSelectedStreamRead(ctx, id, true, apply)
}
func (s *Store) withSelectedStreamRead(ctx context.Context, id string, opaque bool, apply func(context.Context, streamDataNamespace, *store.Run) error) error {
	return s.withSelectedStreamReadPolicy(ctx, id, opaque, false, apply)
}
func (s *Store) withSelectedStreamReadPolicy(ctx context.Context, id string, opaque, retained bool, apply func(context.Context, streamDataNamespace, *store.Run) error) error {
	if ctx == nil {
		return store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session, err := s.client.StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		parent, _, err := s.readRunFormatObservationPolicy(sc, id, false, retained)
		if err == nil && parent != nil && parent.DeletedAt != nil && (!opaque || parent.DataProtocol != store.HistoricalRunData) {
			return nil, store.ErrRunDeleted
		}
		if err != nil {
			return nil, err
		}
		ns, err := s.streamNamespace(sc, id, parent)
		if err != nil {
			return nil, err
		}
		return nil, apply(sc, ns, parent)
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()))
	if err != nil {
		return err
	}
	return ctx.Err()
}
func (s *Store) ensureStreamIndexes(ctx context.Context, days int) error {
	for _, domain := range []struct {
		name, position string
		retained       bool
		collection     *mongo.Collection
	}{{colIncarnationEvents, "seq", false, s.db.Collection(colIncarnationEvents)}, {colIncarnationLogs, "offset", false, s.db.Collection(colIncarnationLogs)}, {colIncarnationPlans, "seq", false, s.db.Collection(colIncarnationPlans)}, {colIncarnationNotes, "seq", true, s.db.Collection(colIncarnationNotes)}} {
		indexes := []mongo.IndexModel{{Keys: bson.D{{Key: "data_namespace", Value: 1}, {Key: domain.position, Value: 1}}, Options: options.Index().SetUnique(true).SetName("namespace_position_unique")}, {Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "data_namespace", Value: 1}, {Key: domain.position, Value: 1}}, Options: options.Index().SetName("tenant_namespace_position")}}
		// Operator-authored notes persist for the life of the run, with no TTL.
		if days > 0 && !domain.retained {
			indexes = append(indexes, ttlIndexModel("stream_ttl", days))
		}
		if err := s.ensureIndexes(ctx, domain.collection, domain.name, indexes); err != nil {
			return fmt.Errorf("stream indexes: %w", err)
		}
	}
	return nil
}
