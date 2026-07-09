package mongo

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// Cloud MemoryStore: shared-knowledge spaces persisted in Mongo, with
// document bodies stored INLINE (markdown is capped at 2 MiB, well under
// Mongo's 16 MB document limit). A future binary mode can move bodies to
// blob storage; for V1 inline keeps the adapter self-contained and the
// quota accounting transactional within Mongo.
//
// Quota is enforced at two levels before bytes land: the per-org
// aggregate ceiling (memory_tenant_usage) and the per-space sub-cap
// (memory_spaces.used_bytes). Both use conditional ($expr) updates so an
// over-quota write is denied atomically; a denied/failed write rolls the
// other counter back.

const (
	colMemorySpaces      = "memory_spaces"
	colMemoryDocs        = "memory_docs"
	colMemoryTenantUsage = "memory_tenant_usage"
)

type memSpaceDoc struct {
	ID         string    `bson:"_id"`
	TenantID   string    `bson:"tenant_id,omitempty"`
	Visibility string    `bson:"visibility"`
	ProjectID  string    `bson:"project_id,omitempty"`
	BotID      string    `bson:"bot_id,omitempty"`
	UserID     string    `bson:"user_id,omitempty"`
	Name       string    `bson:"name"`
	QuotaBytes int64     `bson:"quota_bytes"`
	UsedBytes  int64     `bson:"used_bytes"`
	Mode       string    `bson:"mode"`
	Version    int       `bson:"version"`
	CreatedAt  time.Time `bson:"created_at"`
	UpdatedAt  time.Time `bson:"updated_at"`
}

type memDocID struct {
	SpaceID string `bson:"space_id"`
	Path    string `bson:"path"`
}

type memDoc struct {
	ID          memDocID  `bson:"_id"`
	TenantID    string    `bson:"tenant_id,omitempty"`
	SpaceID     string    `bson:"space_id"`
	Path        string    `bson:"path"`
	Title       string    `bson:"title,omitempty"`
	Description string    `bson:"description,omitempty"`
	Tags        []string  `bson:"tags,omitempty"`
	Size        int64     `bson:"size"`
	Checksum    string    `bson:"checksum,omitempty"`
	Revision    int64     `bson:"revision"`
	Content     []byte    `bson:"content"`
	UpdatedBy   string    `bson:"updated_by,omitempty"`
	UpdatedAt   time.Time `bson:"updated_at"`
}

// MongoMemoryStore implements knowledge.MemoryStore over Mongo.
type MongoMemoryStore struct {
	spaces *mongo.Collection
	docs   *mongo.Collection
	tenant *mongo.Collection
}

var _ knowledge.MemoryStore = (*MongoMemoryStore)(nil)

// NewMongoMemoryStore wires the store to a database (reuse Store.DB()).
func NewMongoMemoryStore(db *mongo.Database) *MongoMemoryStore {
	return &MongoMemoryStore{
		spaces: db.Collection(colMemorySpaces),
		docs:   db.Collection(colMemoryDocs),
		tenant: db.Collection(colMemoryTenantUsage),
	}
}

// EnsureSchema creates the indexes idempotently.
func (s *MongoMemoryStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.spaces.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "visibility", Value: 1}, {Key: "updated_at", Value: -1}}, Options: options.Index().SetName("tenant_vis_recent")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("memory: ensure spaces indexes: %w", err)
	}
	if _, err := s.docs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "space_id", Value: 1}, {Key: "updated_at", Value: -1}}, Options: options.Index().SetName("space_recent")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("memory: ensure docs indexes: %w", err)
	}
	return nil
}

func docFilter(spaceID, path string) bson.M {
	return bson.M{"_id": memDocID{SpaceID: spaceID, Path: path}}
}

// validateCloudTenant layers the cloud adapter's tenancy contract on top of
// the generic SpaceRef validation. knowledge.SpaceRef.Validate() deliberately
// allows an empty TenantID ("required in cloud, empty for local single-tenant"
// — see scope.go) and leaves enforcement to the cloud adapter. THIS store is
// that adapter: every non-global space is tenant-scoped, so a missing TenantID
// is a fail-closed error here. Without it, a non-global ref with TenantID==""
// would compute a tenant-less _id (ref.ID()) and read/write a space outside any
// org's isolation boundary — a cross-tenant leak. Only VisibilityGlobal (the
// instance-wide, org-read-only catalogue) legitimately has no tenant.
func validateCloudTenant(ref knowledge.SpaceRef) error {
	if vErr := ref.Validate(); vErr != nil {
		return vErr
	}
	if ref.Visibility != knowledge.VisibilityGlobal && ref.TenantID == "" {
		return fmt.Errorf("knowledge: %q space %q requires a tenant in cloud mode", ref.Visibility, ref.Name)
	}
	return nil
}

// Root returns a mem:// display URI (no IO).
func (s *MongoMemoryStore) Root(ref knowledge.SpaceRef) (string, error) {
	if err := validateCloudTenant(ref); err != nil {
		return "", err
	}
	return "mem://" + string(ref.Visibility) + "/" + ref.ID(), nil
}

// allDocs lists a space's docs sorted by path. withBody=false excludes
// the (up to 2 MiB) markdown body — metadata-only callers (index, list)
// must not pull every body off Mongo just to render a few KB of labels.
func (s *MongoMemoryStore) allDocs(ctx context.Context, spaceID string, withBody bool) ([]memDoc, error) {
	opt := options.Find().SetSort(bson.M{"path": 1})
	if !withBody {
		opt.SetProjection(bson.M{"content": 0})
	}
	cur, err := s.docs.Find(ctx, bson.M{"space_id": spaceID}, opt)
	if err != nil {
		return nil, fmt.Errorf("memory: list docs: %w", err)
	}
	defer cur.Close(ctx)
	var out []memDoc
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("memory: decode docs: %w", err)
	}
	return out, nil
}

func (s *MongoMemoryStore) BuildIndex(ctx context.Context, ref knowledge.SpaceRef) ([]knowledge.IndexEntry, error) {
	if err := validateCloudTenant(ref); err != nil {
		return nil, err
	}
	docs, err := s.allDocs(ctx, ref.ID(), false)
	if err != nil {
		return nil, err
	}
	var out []knowledge.IndexEntry
	for _, d := range docs {
		if filepath.Ext(d.Path) != ".md" {
			continue
		}
		out = append(out, knowledge.IndexEntry{Path: d.Path, Title: d.Title, Description: d.Description, Tags: d.Tags})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *MongoMemoryStore) Autoload(ctx context.Context, ref knowledge.SpaceRef, patterns []string) ([]knowledge.AutoloadEntry, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	if err := validateCloudTenant(ref); err != nil {
		return nil, err
	}
	docs, err := s.allDocs(ctx, ref.ID(), true)
	if err != nil {
		return nil, err
	}
	var out []knowledge.AutoloadEntry
	seen := map[string]bool{}
	for _, d := range docs {
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, d.Path); ok && !seen[d.Path] {
				seen[d.Path] = true
				out = append(out, knowledge.AutoloadEntry{Path: d.Path, Content: d.Content})
				break
			}
		}
	}
	return out, nil
}

func (s *MongoMemoryStore) ListDocuments(ctx context.Context, ref knowledge.SpaceRef, dir string) ([]knowledge.DocumentMeta, error) {
	if err := validateCloudTenant(ref); err != nil {
		return nil, err
	}
	docs, err := s.allDocs(ctx, ref.ID(), false)
	if err != nil {
		return nil, err
	}
	prefix := dir
	if prefix != "" && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	var out []knowledge.DocumentMeta
	for _, d := range docs {
		if prefix != "" && (len(d.Path) < len(prefix) || d.Path[:len(prefix)] != prefix) {
			continue
		}
		out = append(out, metaOf(d))
	}
	return out, nil
}

func (s *MongoMemoryStore) ReadDocument(ctx context.Context, ref knowledge.SpaceRef, path string) (knowledge.Document, error) {
	if err := validateCloudTenant(ref); err != nil {
		return knowledge.Document{}, err
	}
	if err := knowledge.ValidateDocPath(path); err != nil {
		return knowledge.Document{}, err
	}
	d, err := mongoutil.FindOne[memDoc](ctx, s.docs, docFilter(ref.ID(), path),
		fmt.Errorf("%w: %q", knowledge.ErrDocNotFound, path), "memory: read doc")
	if err != nil {
		return knowledge.Document{}, err
	}
	return knowledge.Document{Meta: metaOf(d), Content: d.Content}, nil
}

func metaOf(d memDoc) knowledge.DocumentMeta {
	return knowledge.DocumentMeta{
		Path: d.Path, Title: d.Title, Description: d.Description, Tags: d.Tags,
		Size: d.Size, Checksum: d.Checksum, Revision: d.Revision,
		UpdatedBy: d.UpdatedBy, UpdatedAt: d.UpdatedAt,
	}
}

func (s *MongoMemoryStore) UsageBytes(ctx context.Context, ref knowledge.SpaceRef) (int64, int64, error) {
	if err := validateCloudTenant(ref); err != nil {
		return 0, 0, err
	}
	var sp memSpaceDoc
	err := s.spaces.FindOne(ctx, bson.M{"_id": ref.ID()}).Decode(&sp)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, knowledge.DefaultQuotaFor(ref.Visibility), nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("memory: usage: %w", err)
	}
	return sp.UsedBytes, sp.QuotaBytes, nil
}

func (s *MongoMemoryStore) DeleteDocument(ctx context.Context, ref knowledge.SpaceRef, path string) error {
	if err := validateCloudTenant(ref); err != nil {
		return err
	}
	if err := knowledge.ValidateDocPath(path); err != nil {
		return err
	}
	// Atomically delete and read the size in one op. A separate
	// FindOne→DeleteOne races a concurrent WriteDocument that replaces the
	// doc between the two calls: the quota would then be decremented by the
	// stale (pre-write) size, leaving used_bytes permanently inflated.
	var d memDoc
	res := s.docs.FindOneAndDelete(ctx, docFilter(ref.ID(), path), options.FindOneAndDelete().SetProjection(bson.M{"size": 1}))
	if errors.Is(res.Err(), mongo.ErrNoDocuments) {
		return nil
	}
	if err := res.Decode(&d); err != nil {
		return fmt.Errorf("memory: delete doc: %w", err)
	}
	if d.Size > 0 {
		_, _ = s.bumpSpace(ctx, ref.ID(), -d.Size)
		if ref.TenantID != "" {
			_, _ = s.bumpTenant(ctx, ref.TenantID, -d.Size)
		}
	}
	return nil
}

// WriteDocument is the transactional path. See package comment.
func (s *MongoMemoryStore) WriteDocument(ctx context.Context, ref knowledge.SpaceRef, in knowledge.DocumentInput) (knowledge.DocumentMeta, error) {
	if err := validateCloudTenant(ref); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	if err := knowledge.ValidateDocPath(in.Path); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	newSize := int64(len(in.Content))
	if newSize > knowledge.DefaultMaxDocumentSize {
		return knowledge.DocumentMeta{}, &knowledge.QuotaError{Aggregate: false, Used: 0, Delta: newSize, Quota: knowledge.DefaultMaxDocumentSize}
	}
	spaceID := ref.ID()
	tenantID := ref.TenantID

	if err := s.ensureSpace(ctx, ref); err != nil {
		return knowledge.DocumentMeta{}, err
	}

	// Optimistic-concurrency loop (compare-and-swap on `revision`). Two writers
	// to the same (space, path) used to each FindOne prevRev, each bump the
	// quota counters, then each UNCONDITIONALLY ReplaceOne — collapsing both
	// into one revision (a lost update) and leaving BOTH per-write counter bumps
	// applied even though only one body survived (quota drift). Now the
	// ReplaceOne is conditioned on the revision we read ({_id, revision} filter
	// + upsert): the writer that lost the race hits an E11000 duplicate-key on
	// its insert, rolls its own counter bump back, and — unless the caller
	// pinned ExpectedRev — re-reads the advanced revision and retries. So the
	// surviving write is the ONLY one whose counters persist. ensureSpace runs
	// once above (idempotent); ensureTenant stays in-loop (also idempotent).
	// No fixed retry cap: each CAS round has exactly one winner, so a writer
	// that loses is guaranteed to make progress (another writer advanced the
	// revision) and will win within at most as many rounds as there are live
	// concurrent writers. The principled bound is therefore the caller's
	// context deadline, not an arbitrary attempt count — a count of 5 used to
	// spuriously fail 12-way contention with "lost the revision race".
	for attempt := 0; ; attempt++ {
		var existing memDoc
		var oldSize, prevRev int64
		if err := s.docs.FindOne(ctx, docFilter(spaceID, in.Path), options.FindOne().SetProjection(bson.M{"size": 1, "revision": 1})).Decode(&existing); err == nil {
			oldSize, prevRev = existing.Size, existing.Revision
		}
		if in.ExpectedRev != 0 && in.ExpectedRev != prevRev {
			return knowledge.DocumentMeta{}, fmt.Errorf("memory: revision conflict (expected %d, have %d)", in.ExpectedRev, prevRev)
		}
		delta := newSize - oldSize

		// 1) org aggregate ceiling.
		if tenantID != "" && delta > 0 {
			if err := s.ensureTenant(ctx, tenantID); err != nil {
				return knowledge.DocumentMeta{}, err
			}
			ok, err := s.bumpTenant(ctx, tenantID, delta)
			if err != nil {
				return knowledge.DocumentMeta{}, err
			}
			if !ok {
				used, quota := s.readUsage(ctx, s.tenant, tenantID, knowledge.DefaultOrgAggregateQuota)
				return knowledge.DocumentMeta{}, &knowledge.QuotaError{Aggregate: true, Used: used, Delta: delta, Quota: quota}
			}
		}

		// 2) per-space sub-cap.
		if delta > 0 {
			ok, err := s.bumpSpace(ctx, spaceID, delta)
			if err != nil || !ok {
				if tenantID != "" {
					_, _ = s.bumpTenant(ctx, tenantID, -delta) // rollback aggregate
				}
				if err != nil {
					return knowledge.DocumentMeta{}, err
				}
				used, quota := s.readUsage(ctx, s.spaces, spaceID, 0)
				return knowledge.DocumentMeta{}, &knowledge.QuotaError{Aggregate: false, Used: used, Delta: delta, Quota: quota}
			}
		} else if delta < 0 {
			_, _ = s.bumpSpace(ctx, spaceID, delta)
			if tenantID != "" {
				_, _ = s.bumpTenant(ctx, tenantID, delta)
			}
		}

		// 3) compare-and-swap upsert: matches (and replaces) only if the on-disk
		// revision is still prevRev. A mismatch (a concurrent writer advanced it,
		// or inserted first) makes the upsert attempt an insert of an
		// already-present _id => E11000 duplicate-key, which we treat as a lost CAS.
		title, desc, tags := knowledge.ParseMarkdownMeta(in.Content)
		now := time.Now().UTC()
		doc := memDoc{
			ID: memDocID{SpaceID: spaceID, Path: in.Path}, TenantID: tenantID, SpaceID: spaceID,
			Path: in.Path, Title: title, Description: desc, Tags: tags,
			Size: newSize, Checksum: knowledge.ChecksumHex(in.Content), Revision: prevRev + 1,
			Content: in.Content, UpdatedBy: in.UpdatedBy, UpdatedAt: now,
		}
		casFilter := bson.M{"_id": memDocID{SpaceID: spaceID, Path: in.Path}, "revision": prevRev}
		_, err := s.docs.ReplaceOne(ctx, casFilter, doc, options.Replace().SetUpsert(true))
		if err == nil {
			return metaOf(doc), nil
		}

		// Failed write — roll this attempt's counter bumps back so a retry (or
		// the returned error) leaves the counters consistent with what is on disk.
		if delta != 0 {
			_, _ = s.bumpSpace(ctx, spaceID, -delta)
			if tenantID != "" {
				_, _ = s.bumpTenant(ctx, tenantID, -delta)
			}
		}
		if mongo.IsDuplicateKeyError(err) {
			// Lost the CAS: another writer advanced the revision between our
			// FindOne and our ReplaceOne.
			if in.ExpectedRev != 0 {
				return knowledge.DocumentMeta{}, fmt.Errorf("memory: revision conflict (expected %d, lost a concurrent write)", in.ExpectedRev)
			}
			// Retry until we win or the caller's context is done; progress is
			// guaranteed each round, so the context deadline is the only bound.
			if cerr := ctx.Err(); cerr != nil {
				return knowledge.DocumentMeta{}, fmt.Errorf("memory: write doc: lost the revision race (%d attempts) before the context was done: %w", attempt+1, cerr)
			}
			continue // re-read the advanced revision and retry
		}
		return knowledge.DocumentMeta{}, fmt.Errorf("memory: write doc: %w", err)
	}
}

// ---- quota helpers ----

func (s *MongoMemoryStore) ensureTenant(ctx context.Context, tenant string) error {
	_, err := s.tenant.UpdateOne(ctx, bson.M{"_id": tenant},
		bson.M{"$setOnInsert": bson.M{"used_bytes": int64(0), "quota_bytes": knowledge.DefaultOrgAggregateQuota}},
		options.UpdateOne().SetUpsert(true))
	return err
}

// SetTenantQuota sets the org-aggregate memory ceiling for a tenant,
// preserving the running used_bytes. The super-admin org console calls
// this when an operator changes Team.MemoryQuotaBytes — without it, the
// override was persisted on the Team but never reached the counter the
// CAS in bumpCounter actually enforces (so it had no effect). A
// quotaBytes <= 0 resets to the platform default. Upserts so an org can
// be capped before it has written any memory.
func (s *MongoMemoryStore) SetTenantQuota(ctx context.Context, tenant string, quotaBytes int64) error {
	if quotaBytes <= 0 {
		quotaBytes = knowledge.DefaultOrgAggregateQuota
	}
	_, err := s.tenant.UpdateOne(ctx, bson.M{"_id": tenant},
		bson.M{
			"$set":         bson.M{"quota_bytes": quotaBytes},
			"$setOnInsert": bson.M{"used_bytes": int64(0)},
		},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("memory: set tenant quota: %w", err)
	}
	return nil
}

// TenantUsedBytes reports the org-aggregate memory consumption — the
// same running counter the write-path CAS enforces. Consumed by the
// usage REST views; an org with no memory activity reads 0.
func (s *MongoMemoryStore) TenantUsedBytes(ctx context.Context, tenant string) (int64, error) {
	var doc struct {
		UsedBytes int64 `bson:"used_bytes"`
	}
	err := s.tenant.FindOne(ctx, bson.M{"_id": tenant}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("memory: tenant used bytes: %w", err)
	}
	return doc.UsedBytes, nil
}

func (s *MongoMemoryStore) bumpTenant(ctx context.Context, tenant string, delta int64) (bool, error) {
	return s.bumpCounter(ctx, s.tenant, tenant, delta, false)
}

func (s *MongoMemoryStore) ensureSpace(ctx context.Context, ref knowledge.SpaceRef) error {
	now := time.Now().UTC()
	_, err := s.spaces.UpdateOne(ctx, bson.M{"_id": ref.ID()}, bson.M{
		"$setOnInsert": bson.M{
			"tenant_id": ref.TenantID, "visibility": string(ref.Visibility),
			"project_id": ref.ProjectID, "bot_id": ref.BotID, "user_id": ref.UserID,
			"name": ref.Name, "used_bytes": int64(0),
			"quota_bytes": knowledge.DefaultQuotaFor(ref.Visibility),
			"mode":        "markdown", "version": 1, "created_at": now,
		},
		"$set": bson.M{"updated_at": now},
	}, options.UpdateOne().SetUpsert(true))
	return err
}

func (s *MongoMemoryStore) bumpSpace(ctx context.Context, spaceID string, delta int64) (bool, error) {
	// allowZeroQuota: a space quota_bytes of 0 means "no sub-cap" (global).
	return s.bumpCounter(ctx, s.spaces, spaceID, delta, true)
}

// bumpCounter applies a byte delta to a used_bytes counter under its
// cap. A non-positive delta always succeeds (and clamps any drift back
// to 0); a positive delta is gated by the $expr CAS, with allowZeroQuota
// treating quota_bytes==0 as "unlimited". Returns whether it applied.
func (s *MongoMemoryStore) bumpCounter(ctx context.Context, coll *mongo.Collection, id string, delta int64, allowZeroQuota bool) (bool, error) {
	if delta <= 0 {
		_, err := coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$inc": bson.M{"used_bytes": delta}})
		_ = s.clampUsage(ctx, coll, id)
		return true, err
	}
	filter := bson.M{"_id": id, "$expr": lteAddExpr(delta)}
	if allowZeroQuota {
		filter = bson.M{"_id": id, "$or": []bson.M{{"quota_bytes": int64(0)}, {"$expr": lteAddExpr(delta)}}}
	}
	res, err := coll.UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"used_bytes": delta}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount == 1, nil
}

// lteAddExpr builds {$lte: [{$add: ["$used_bytes", delta]}, "$quota_bytes"]}.
func lteAddExpr(delta int64) bson.M {
	return bson.M{"$lte": bson.A{bson.M{"$add": bson.A{"$used_bytes", delta}}, "$quota_bytes"}}
}

// clampUsage resets a negative used_bytes (accounting drift) back to 0.
func (s *MongoMemoryStore) clampUsage(ctx context.Context, coll *mongo.Collection, id string) error {
	_, err := coll.UpdateOne(ctx, bson.M{"_id": id, "used_bytes": bson.M{"$lt": 0}}, bson.M{"$set": bson.M{"used_bytes": int64(0)}})
	return err
}

// readUsage returns (used, quota) for a counter doc, or (0, missQuota)
// when absent. Used by the quota-deny error paths for both counters.
func (s *MongoMemoryStore) readUsage(ctx context.Context, coll *mongo.Collection, id string, missQuota int64) (used, quota int64) {
	var doc struct {
		Used  int64 `bson:"used_bytes"`
		Quota int64 `bson:"quota_bytes"`
	}
	if err := coll.FindOne(ctx, bson.M{"_id": id}).Decode(&doc); err != nil {
		return 0, missQuota
	}
	return doc.Used, doc.Quota
}
