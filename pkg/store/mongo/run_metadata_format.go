package mongo

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A scalar projection can hide repeated BSON keys. Preserve their cardinality
// before selecting a header. Keep decoder-recognized aliases too: projecting
// them away would turn a refusal into a canonical or absent selector. Explicit
// character classes cover JSON EqualFold and BSON ToLower (long-s and dotted-I)
// without relying on Mongo Unicode case conversion. Payloads are not traversed.
// `\z` ends it, as every key pattern of this package (tenantHeaderAliasPattern
// says why).
const runFormatHeaderAliasPattern = `^[fF][oO][rR][mM][aA][tT]_[vV][eE][rR]([sSſ][iI]|[sS][iIİ])[oO][nN]\z`

func valueFormatHeaderEntries() bson.M {
	return bson.M{"$filter": bson.M{
		"input": bson.M{"$objectToArray": "$$ROOT"}, "as": "header",
		"cond": bson.M{"$regexMatch": bson.M{"input": "$$header.k", "regex": runFormatHeaderAliasPattern}},
	}}
}

// canonicalValueFormatPredicate is evaluated by the mutating command, not only
// by a preflight read. Neither arrays nor duplicate BSON headers can match.
func canonicalValueFormatPredicate(format store.ValueFormat) bson.M {
	var accepted bson.A
	switch format {
	case store.LegacyValueFormat:
		accepted = bson.A{0, 1}
	case store.ExactValueFormat:
		accepted = bson.A{2}
	default:
		return bson.M{"$expr": false}
	}
	singleton := bson.M{"$let": bson.M{
		"vars": bson.M{"value": bson.M{"$getField": bson.M{"field": "v", "input": bson.M{"$arrayElemAt": bson.A{"$$headers", 0}}}}},
		"in": bson.M{"$and": bson.A{
			bson.M{"$eq": bson.A{bson.M{"$getField": bson.M{"field": "k", "input": bson.M{"$arrayElemAt": bson.A{"$$headers", 0}}}}, "format_version"}},
			bson.M{"$in": bson.A{bson.M{"$type": "$$value"}, bson.A{"int", "long"}}},
			bson.M{"$in": bson.A{"$$value", accepted}},
		}},
	}}
	return bson.M{"$expr": bson.M{"$let": bson.M{
		"vars": bson.M{"headers": valueFormatHeaderEntries()},
		"in": bson.M{"$switch": bson.M{
			"branches": bson.A{
				bson.M{"case": bson.M{"$eq": bson.A{bson.M{"$size": "$$headers"}, 0}}, "then": format == store.LegacyValueFormat},
				bson.M{"case": bson.M{"$eq": bson.A{bson.M{"$size": "$$headers"}, 1}}, "then": singleton},
			}, "default": false,
		}},
	}}}
}

// runFormatMetadataProjection preserves duplicate header cardinality before
// projecting ordinary metadata. Deletion uses the same parser as live writers.
func runFormatMetadataProjection() bson.M {
	return bson.M{"_id": 1, "version": 1, "deleted_at": 1, "status": 1, "tenant_id": 1,
		valueCleanupPendingField: 1, "value_format_headers": valueFormatHeaderEntries(),
		"data_headers": runDataHeaderProjection()["data_headers"]}
}

func decodeRunFormatMetadata(ctx context.Context, raw bson.Raw) (*store.Run, error) {
	return decodeRunFormatObservation(ctx, raw, false)
}

// capture is reserved for initial subscription observation after tenant-scoped
// metadata lookup. Existing bound readers keep their original validation path.
func decodeRunFormatObservation(ctx context.Context, raw bson.Raw, capture bool) (*store.Run, error) {
	values, err := raw.Lookup("value_format_headers").Array().Values()
	if err != nil {
		return nil, err
	}
	headers := make(bson.D, 0, len(values))
	for _, entry := range values {
		headers = append(headers, bson.E{Key: entry.Document().Lookup("k").StringValue(), Value: entry.Document().Lookup("v")})
	}
	encoded, err := bson.Marshal(headers)
	if err != nil {
		return nil, err
	}
	if _, err = store.RunValueFormatFromBSON(ctx, encoded); err != nil {
		return nil, err
	}
	var header struct {
		FormatVersion int `bson:"format_version"`
	}
	if err = bson.Unmarshal(encoded, &header); err != nil {
		return nil, err
	}
	dataEntries, ok := raw.Lookup("data_headers").ArrayOK()
	if !ok {
		return nil, store.ErrInvalidRunDataIdentity
	}
	entries, err := dataEntries.Values()
	if err != nil {
		return nil, err
	}
	dataHeaders := bson.D{}
	for _, entry := range entries {
		document, ok := entry.DocumentOK()
		if !ok {
			return nil, store.ErrInvalidRunDataIdentity
		}
		key, ok := document.Lookup("k").StringValueOK()
		if !ok {
			return nil, store.ErrInvalidRunDataIdentity
		}
		dataHeaders = append(dataHeaders, bson.E{Key: key, Value: document.Lookup("v")})
	}
	dataEncoded, err := bson.Marshal(dataHeaders)
	if err != nil {
		return nil, err
	}
	dataHeader, err := store.RunDataHeaderFromBSON(ctx, dataEncoded)
	if err != nil {
		return nil, err
	}
	if err := store.RequireRunDataStorage(dataHeader.Protocol); err != nil {
		return nil, err
	}
	if capture {
		if _, bound := store.RunDataBindingFrom(ctx); !bound {
			binding, e := store.CaptureRunDataBinding(dataHeader)
			if e != nil {
				return nil, e
			}
			ctx, e = store.WithRunDataBinding(ctx, binding)
			if e != nil {
				return nil, e
			}
		}
	}
	if err := store.CheckRunDataBinding(ctx, dataHeader); err != nil {
		return nil, err
	}
	var parent store.Run
	if err = bson.Unmarshal(raw, &parent); err != nil {
		return nil, err
	}
	parent.FormatVersion = header.FormatVersion
	parent.BirthID = dataHeader.BirthID
	parent.DataProtocol = dataHeader.Protocol
	if parent.ID != dataHeader.RunID {
		return nil, store.ErrRunDataIdentityChanged
	}
	return &parent, nil
}

// readRunFormatMetadata includes tombstones without decoding payload values.
func (s *Store) readRunFormatMetadata(ctx context.Context, id string) (*store.Run, bson.Raw, error) {
	return s.readRunFormatObservation(ctx, id, false)
}
func (s *Store) readRunFormatObservation(ctx context.Context, id string, capture bool) (*store.Run, bson.Raw, error) {
	return s.readRunFormatObservationPolicy(ctx, id, capture, false)
}

// readRunFormatObservationPolicy is the read every Mongo stream read, scan
// batch and metadata read goes through. retained asks for the canonical tenant
// (parent.TenantID). A binding that observed a tenant forces it too and is
// compared with it after the header: another tenant under the same header
// answers ErrRunDataIdentityChanged, whatever the tenant filter lets through.
// Under such a binding a tenant the strict parser refuses is reported after
// the header comparison too, so a successor answers ErrRunDataIdentityChanged
// whatever tenant shape it carries — the answer LoadRun gives.
func (s *Store) readRunFormatObservationPolicy(ctx context.Context, id string, capture, retained bool) (*store.Run, bson.Raw, error) {
	return readRunMetadataProjection(ctx, s.runs, id, capture, retained, runFormatMetadataProjection())
}

func readRunMetadataProjection(ctx context.Context, runs *mongo.Collection, id string, capture, retained bool, projection bson.M) (*store.Run, bson.Raw, error) {
	if ctx == nil {
		return nil, nil, store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	observed := observesTenant(ctx)
	retained = retained || observed
	if retained {
		// The canonical tenant replaces the decoded one, so the plain field is
		// not projected: a tenant_id of another BSON type is judged by the
		// strict parser alone (ErrInvalidRunDataIdentity), never by the
		// struct decoder's untyped error.
		delete(projection, "tenant_id")
		projection["retained_tenant_headers"] = tenantHeaderEntries()
	}
	cursor, err := runs.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: withTenantFilter(ctx, bson.M{"_id": id})}},
		{{Key: "$project", Value: projection}},
	})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, store.CheckMissingRunDataParent(ctx, id)
	}
	raw := append(bson.Raw(nil), cursor.Current...)
	var tenant string
	var tenantErr error
	if retained {
		tenant, tenantErr = retainedCanonicalTenant(raw)
		if tenantErr != nil && !observed {
			return nil, raw, tenantErr
		}
	}
	parent, err := decodeRunFormatObservation(ctx, raw, capture)
	if err != nil {
		return parent, raw, err
	}
	if tenantErr != nil {
		return nil, raw, tenantErr
	}
	if retained {
		parent.TenantID = tenant
	}
	if observed {
		if err := store.CheckRunObservedTenant(ctx, tenant); err != nil {
			return nil, raw, err
		}
	}
	return parent, raw, nil
}

func (s *Store) readRunFormatParent(ctx context.Context, id string) (*store.Run, error) {
	parent, _, err := s.readRunFormatMetadata(ctx, id)
	if err != nil || parent == nil {
		return parent, err
	}
	if parent.DeletedAt != nil {
		return nil, fmt.Errorf("store/mongo: run %s: %w", id, store.ErrRunDeleted)
	}
	return parent, nil
}

// runMetadataFormat holds an operation's original format and, when its context
// carries a run data binding, the birth that binding observed. It is not
// execution authority. Preparing again in a retry would incorrectly adopt a
// successor.
type runMetadataFormat struct {
	original store.RunValueFormat
	// birth is the bound birth, "" included (the historical empty birth);
	// bound tells it from an unbound write, which carries no birth predicate.
	birth string
	bound bool
}

func (s *Store) prepareRunMetadataFormat(ctx context.Context, id string) (runMetadataFormat, error) {
	g, err := s.prepareRunMetadataValueFormat(ctx, id)
	if err != nil {
		return runMetadataFormat{}, err
	}
	if b, bound := store.RunDataBindingFrom(ctx); bound {
		g.birth, g.bound = b.Header().BirthID, true
	}
	return g, nil
}

func (s *Store) prepareRunMetadataValueFormat(ctx context.Context, id string) (runMetadataFormat, error) {
	parent, err := s.valueParent(ctx, id)
	if err != nil {
		return runMetadataFormat{}, err
	}
	if parent == nil {
		// This is a noncreating mutation. An explicit binding remains explicit;
		// absent bindings retain legacy intent even if birth races the command.
		format, err := store.ValueFormatForWrite(ctx, id)
		return runMetadataFormat{original: store.RunValueFormat{RunID: id, Format: format}}, err
	}
	if _, err = store.CheckRunFormatReplacement(ctx, parent, parent); err != nil {
		return runMetadataFormat{}, err
	}
	original, err := store.CaptureRunValueFormat(parent)
	return runMetadataFormat{original: original}, err
}

// filter evaluates the original format, and under a binding the bound birth,
// inside the mutating command: a successor recreated under the same ID after
// the guard's read matches nothing. An unbound write keeps the format
// predicate alone and lands on whatever run holds the ID.
func (g runMetadataFormat) filter(ctx context.Context, caller bson.M) bson.M {
	clauses := bson.A{caller, canonicalValueFormatPredicate(g.original.Format)}
	if g.bound {
		clauses = append(clauses, canonicalBirthPredicate(g.birth))
	}
	return notDeleted(withTenantFilter(ctx, bson.M{
		"_id":  g.original.RunID,
		"$and": clauses,
	}))
}

// checkMiss classifies the format boundary, and under a binding the identity:
// its read compares the carried binding, so a bound miss on another birth
// answers ErrRunDataIdentityChanged and a header the parser refuses answers
// the parser's error. A known-format business CAS miss (including a genuinely
// absent run) retains the caller's old result.
func (g runMetadataFormat) checkMiss(ctx context.Context, s *Store) error {
	parent, err := s.valueParent(ctx, g.original.RunID)
	if err != nil || parent == nil {
		return err
	}
	current, err := store.CaptureRunValueFormat(parent)
	if err != nil {
		return err
	}
	if current != g.original {
		return fmt.Errorf("%w: original %d, current %d", store.ErrValueFormatChanged, g.original.Format, current.Format)
	}
	return nil
}

// updateRunMetadata does not upsert and does not retry the caller's mutation.
func (s *Store) updateRunMetadata(ctx context.Context, id string, filter bson.M, update any) (*mongo.UpdateResult, error) {
	guard, err := s.prepareRunMetadataFormat(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.afterRunMetadataPrepare != nil {
		s.afterRunMetadataPrepare(id)
	}
	result, err := s.runs.UpdateOne(ctx, guard.filter(ctx, filter), update)
	if err != nil {
		return nil, err
	}
	if result.MatchedCount == 0 {
		err = guard.checkMiss(ctx, s)
	}
	return result, err
}

func (s *Store) findOneAndUpdateRunMetadata(ctx context.Context, id string, filter bson.M, update any, opts ...options.Lister[options.FindOneAndUpdateOptions]) (*mongo.SingleResult, error) {
	var configured options.FindOneAndUpdateOptions
	for _, option := range opts {
		for _, apply := range option.List() {
			if err := apply(&configured); err != nil {
				return nil, err
			}
		}
	}
	if configured.Upsert != nil && *configured.Upsert {
		return nil, fmt.Errorf("%w: metadata mutation cannot create a run", store.ErrInvalidValueFormat)
	}
	guard, err := s.prepareRunMetadataFormat(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.afterRunMetadataPrepare != nil {
		s.afterRunMetadataPrepare(id)
	}
	result := s.runs.FindOneAndUpdate(ctx, guard.filter(ctx, filter), update, opts...)
	if err = result.Err(); err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, err
		}
		if err = guard.checkMiss(ctx, s); err != nil {
			return nil, err
		}
	}
	return result, nil
}
