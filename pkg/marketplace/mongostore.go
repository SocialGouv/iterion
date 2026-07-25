package marketplace

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// colMarketplace is the Mongo collection holding registry entries.
const colMarketplace = "marketplace"

// MongoStore is the cloud-mode Store. Slug is the _id; ReplaceOne with
// upsert handles submit, and the install counter is bumped via $inc.
// Indexes mirror the JSON store's query surface (text search across
// Name/Description/Tags; popularity sort).
type MongoStore struct{ col *mongo.Collection }

// NewMongoStore wraps a Mongo database into a marketplace.Store.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{col: db.Collection(colMarketplace)}
}

// EnsureSchema creates the marketplace indexes idempotently. Mirrors
// the pattern used by orgusage.EnsureSchema / webhooks.EnsureSchema.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colMarketplace)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Compound index for the popularity sort served by List —
		// (installs desc, slug asc) matches the JSON store's order.
		{Keys: bson.D{{Key: "installs", Value: -1}, {Key: "_id", Value: 1}},
			Options: options.Index().SetName("popularity")},
		// A multikey index on tags powers exact-tag filtering.
		{Keys: bson.D{{Key: "tags", Value: 1}},
			Options: options.Index().SetName("tags")},
		// Mongo text index across the fields List's text filter
		// searches. Lets the query planner serve large registries
		// without a full-collection scan; the JSON store equivalent
		// is a linear scan since the dataset there is tiny.
		{Keys: bson.D{
			{Key: "name", Value: "text"},
			{Key: "display_name", Value: "text"},
			{Key: "description", Value: "text"},
			{Key: "author", Value: "text"},
			{Key: "tags", Value: "text"},
		}, Options: options.Index().SetName("text_search")},
		// Browse visibility: scope + status + popularity sort.
		{Keys: bson.D{{Key: "scope", Value: 1}, {Key: "status", Value: 1}, {Key: "installs", Value: -1}},
			Options: options.Index().SetName("scope_status")},
		// Org-scoped browse + org moderation queue.
		{Keys: bson.D{{Key: "org_id", Value: 1}, {Key: "status", Value: 1}},
			Options: options.Index().SetName("org_status")},
		// Moderation queue (oldest pending first).
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: 1}},
			Options: options.Index().SetName("moderation_queue")},
		// "My submissions" lookup.
		{Keys: bson.D{{Key: "submitted_by", Value: 1}},
			Options: options.Index().SetName("submitter")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("marketplace: ensure indexes: %w", err)
	}
	return nil
}

// List returns every entry matching q, ordered per q.Sort (default:
// Installs desc, then Slug asc) — same shape as JSONStore.List. The
// viewer's scope/status reach (q.Viewer) is composed into the filter so
// the database, not the handler, enforces visibility.
func (s *MongoStore) List(ctx context.Context, q Query) ([]Entry, error) {
	if err := ValidateSort(q.Sort); err != nil {
		return nil, err
	}
	and := bson.A{}
	if t := q.Tag; t != "" {
		and = append(and, bson.M{"tags": t})
	}
	switch q.Kind {
	case KindPlugin:
		and = append(and, bson.M{"kind": string(KindPlugin)})
	case KindBot:
		// Legacy entries have no kind field; treat missing/empty as bot.
		and = append(and, bson.M{"$or": bson.A{
			bson.M{"kind": string(KindBot)},
			bson.M{"kind": bson.M{"$in": bson.A{"", nil}}},
			bson.M{"kind": bson.M{"$exists": false}},
		}})
	}
	if text := q.Text; text != "" {
		// Case-insensitive SUBSTRING match across the searchable fields,
		// mirroring the JSON store's behaviour. We deliberately avoid Mongo's
		// $text operator: $text inside an $or cannot be co-planned with the
		// viewer scope/status $and/$or clauses (Mongo returns
		// NoQueryExecutionPlans), and a stemmed word index also misses the
		// partial-token matches users expect while typing. QuoteMeta keeps the
		// query literal (no regex injection / ReDoS).
		rx := bson.M{"$regex": regexp.QuoteMeta(text), "$options": "i"}
		and = append(and, bson.M{"$or": bson.A{
			bson.M{"_id": rx},
			bson.M{"name": rx},
			bson.M{"display_name": rx},
			bson.M{"description": rx},
			bson.M{"tags": rx},
		}})
	}
	if vf := viewerFilter(q.Viewer); vf != nil {
		and = append(and, vf)
	}
	filter := bson.M{}
	if len(and) > 0 {
		filter["$and"] = and
	}
	if q.Sort == SortName {
		return s.listSortedByName(ctx, filter)
	}
	var sortSpec bson.D
	switch q.Sort {
	case SortRecent:
		sortSpec = bson.D{{Key: "updated_at", Value: -1}, {Key: "_id", Value: 1}}
	default: // "" | SortPopular
		sortSpec = bson.D{{Key: "installs", Value: -1}, {Key: "_id", Value: 1}}
	}
	cur, err := s.col.Find(ctx, filter, options.Find().SetSort(sortSpec))
	if err != nil {
		return nil, fmt.Errorf("marketplace: find: %w", err)
	}
	defer cur.Close(ctx)
	var out []Entry
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("marketplace: decode: %w", err)
	}
	return out, nil
}

// listSortedByName serves SortName via an aggregation: the comparison key
// is DisplayName-or-Name lowercased (JSONStore.sortNameOf parity), which a
// plain Find sort spec can't express. Slug (_id) breaks ties.
func (s *MongoStore) listSortedByName(ctx context.Context, filter bson.M) ([]Entry, error) {
	displayName := bson.M{"$ifNull": bson.A{"$display_name", ""}}
	sortName := bson.M{"$toLower": bson.M{"$cond": bson.A{
		bson.M{"$eq": bson.A{displayName, ""}},
		"$name",
		"$display_name",
	}}}
	cur, err := s.col.Aggregate(ctx, bson.A{
		bson.M{"$match": filter},
		bson.M{"$addFields": bson.M{"_sort_name": sortName}},
		bson.M{"$sort": bson.D{{Key: "_sort_name", Value: 1}, {Key: "_id", Value: 1}}},
		bson.M{"$unset": "_sort_name"},
	})
	if err != nil {
		return nil, fmt.Errorf("marketplace: name-sorted find: %w", err)
	}
	defer cur.Close(ctx)
	var out []Entry
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("marketplace: decode: %w", err)
	}
	return out, nil
}

func (s *MongoStore) Get(ctx context.Context, slug string) (*Entry, bool, error) {
	var e Entry
	err := s.col.FindOne(ctx, bson.M{"_id": slug}).Decode(&e)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("marketplace: get: %w", err)
	}
	return &e, true, nil
}

// Upsert replaces (or inserts) the entry keyed by Slug. When the
// caller passes a zero Installs (submit-form case) the existing
// counter is preserved via a follow-up $set excluding it; in the
// common "first submit" case the document doesn't exist yet, so the
// $setOnInsert path seeds it to 0.
func (s *MongoStore) Upsert(ctx context.Context, e Entry) error {
	if e.Slug == "" {
		return errors.New("marketplace: slug required")
	}
	set := bson.M{
		"kind":          string(e.Kind),
		"categories":    e.Categories,
		"name":          e.Name,
		"display_name":  e.DisplayName,
		"icon":          e.Icon,
		"description":   e.Description,
		"author":        e.Author,
		"tags":          e.Tags,
		"repo_url":      e.RepoURL,
		"ref":           e.Ref,
		"subpath":       e.Subpath,
		"version":       e.Version,
		"readme":        e.README,
		"presets":       e.Presets,
		"updated_at":    e.UpdatedAt,
		"scope":         string(e.Scope),
		"org_id":        e.OrgID,
		"status":        string(e.Status),
		"source":        string(e.Source),
		"bundle_ref":    e.BundleRef,
		"submitted_by":  e.SubmittedBy,
		"reviewed_by":   e.ReviewedBy,
		"reviewed_at":   e.ReviewedAt,
		"reject_reason": e.RejectReason,
	}
	setOnInsert := bson.M{
		"installs":   0,
		"created_at": e.CreatedAt,
	}
	if e.Installs > 0 {
		// Caller (e.g. an admin reseed) explicitly carries an install
		// count over — honor it.
		set["installs"] = e.Installs
		delete(setOnInsert, "installs")
	}
	_, err := s.col.UpdateOne(ctx,
		bson.M{"_id": e.Slug},
		bson.M{"$set": set, "$setOnInsert": setOnInsert},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("marketplace: upsert: %w", err)
	}
	return nil
}

// IncrementInstalls bumps installs by 1. Returns an error when slug is
// unknown; the install endpoint validates the entry exists before
// calling.
func (s *MongoStore) IncrementInstalls(ctx context.Context, slug string) error {
	return mongoutil.UpdateOneChecked(ctx, s.col, bson.M{"_id": slug}, bson.M{"$inc": bson.M{"installs": 1}},
		fmt.Errorf("marketplace: entry %q not found", slug), "marketplace: increment installs")
}

// SetStatus transitions slug's moderation status. expect, when set, is a
// CAS guard composed into the match so concurrent moderators can't both
// win; a guard miss returns ErrStatusConflict. Review metadata is
// stamped in the same update.
func (s *MongoStore) SetStatus(ctx context.Context, slug string, expect, next Status, review Review) error {
	match := bson.M{"_id": slug}
	if expect != "" {
		// Match the expected status, treating empty/absent as approved
		// (EffectiveStatus parity) so legacy entries CAS correctly.
		if expect == StatusApproved {
			match["$or"] = bson.A{
				bson.M{"status": string(StatusApproved)},
				bson.M{"status": bson.M{"$in": bson.A{"", nil}}},
				bson.M{"status": bson.M{"$exists": false}},
			}
		} else {
			match["status"] = string(expect)
		}
	}
	set := bson.M{
		"status":      string(next),
		"reviewed_by": review.By,
		"reviewed_at": review.At,
	}
	if next == StatusRejected {
		set["reject_reason"] = review.Reason
	} else {
		set["reject_reason"] = ""
	}
	res, err := s.col.UpdateOne(ctx, match, bson.M{"$set": set})
	if err != nil {
		return fmt.Errorf("marketplace: set status: %w", err)
	}
	if res.MatchedCount == 0 {
		// Distinguish "no such slug" from "CAS guard missed".
		if _, ok, gerr := s.Get(ctx, slug); gerr == nil && ok {
			return ErrStatusConflict
		}
		return fmt.Errorf("marketplace: entry %q not found", slug)
	}
	return nil
}

// ListForModeration returns entries in the requested moderation states
// (default {StatusPending}), scoped to q.OrgIDs unless q.All is set,
// sorted oldest-first.
func (s *MongoStore) ListForModeration(ctx context.Context, q ModerationQuery) ([]Entry, error) {
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = []Status{StatusPending}
	}
	statusVals := make(bson.A, 0, len(statuses)+2)
	for _, st := range statuses {
		statusVals = append(statusVals, string(st))
		if st == StatusApproved {
			// legacy/absent status counts as approved
			statusVals = append(statusVals, "", nil)
		}
	}
	and := bson.A{bson.M{"status": bson.M{"$in": statusVals}}}
	if !q.All {
		and = append(and, bson.M{"org_id": bson.M{"$in": toAny(q.OrgIDs)}})
	}
	cur, err := s.col.Find(ctx, bson.M{"$and": and},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("marketplace: moderation find: %w", err)
	}
	defer cur.Close(ctx)
	var out []Entry
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("marketplace: moderation decode: %w", err)
	}
	return out, nil
}

// Delete removes slug. A missing slug is not an error.
func (s *MongoStore) Delete(ctx context.Context, slug string) error {
	if _, err := s.col.DeleteOne(ctx, bson.M{"_id": slug}); err != nil {
		return fmt.Errorf("marketplace: delete: %w", err)
	}
	return nil
}

// viewerFilter builds the bson visibility constraint mirroring
// types.Visible. Returns nil when v.Enforce is false (local mode → no
// filtering).
func viewerFilter(v ViewerContext) bson.M {
	if !v.Enforce {
		return nil
	}
	approved := bson.M{"$or": bson.A{
		bson.M{"status": string(StatusApproved)},
		bson.M{"status": bson.M{"$in": bson.A{"", nil}}},
		bson.M{"status": bson.M{"$exists": false}},
	}}
	visible := bson.A{}
	if v.IsSuperAdmin {
		visible = append(visible, approved)
	} else {
		scopeOpts := bson.A{
			bson.M{"scope": string(ScopePublic)},
			bson.M{"scope": bson.M{"$in": bson.A{"", nil}}},
			bson.M{"scope": bson.M{"$exists": false}},
		}
		if v.Authenticated {
			scopeOpts = append(scopeOpts, bson.M{"scope": string(ScopeInstance)})
		}
		if len(v.OrgIDs) > 0 {
			scopeOpts = append(scopeOpts, bson.M{"$and": bson.A{
				bson.M{"scope": string(ScopeOrg)},
				bson.M{"org_id": bson.M{"$in": toAny(v.OrgIDs)}},
			}})
		}
		visible = append(visible, bson.M{"$and": bson.A{approved, bson.M{"$or": scopeOpts}}})
	}
	if v.Authenticated && v.UserID != "" {
		visible = append(visible, bson.M{"submitted_by": v.UserID})
	}
	return bson.M{"$or": visible}
}

// toAny converts a []string to a bson.A for $in clauses.
func toAny(in []string) bson.A {
	out := make(bson.A, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
