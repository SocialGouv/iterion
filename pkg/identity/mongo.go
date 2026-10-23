package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection names. Pinned constants so monitoring + migration
// tooling have a stable target.
const (
	colUsers          = "users"
	colOrgs           = "orgs"
	colOrgMemberships = "org_memberships"
	colTeams          = "teams"
	colMemberships    = "memberships"
	colInvitations    = "invitations"
	colOIDCLinks      = "oidc_links"
)

// MongoStore implements Store on top of MongoDB.
type MongoStore struct {
	db             *mongo.Database
	users          *mongo.Collection
	orgs           *mongo.Collection
	orgMemberships *mongo.Collection
	teams          *mongo.Collection
	memberships    *mongo.Collection
	invitations    *mongo.Collection
	oidcLinks      *mongo.Collection
}

// NewMongoStore returns a MongoStore wired to the given database.
// EnsureSchema must be called once before serving traffic.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{
		db:             db,
		users:          db.Collection(colUsers),
		orgs:           db.Collection(colOrgs),
		orgMemberships: db.Collection(colOrgMemberships),
		teams:          db.Collection(colTeams),
		memberships:    db.Collection(colMemberships),
		invitations:    db.Collection(colInvitations),
		oidcLinks:      db.Collection(colOIDCLinks),
	}
}

// EnsureSchema creates required indexes idempotently. Safe to run on
// every server boot.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.users.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetName("email_unique")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure users indexes: %w", err)
	}
	if _, err := s.orgs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("slug_unique")},
		{Keys: bson.D{{Key: "created_at", Value: -1}}, Options: options.Index().SetName("created_desc")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
		{Keys: bson.D{{Key: "migrated_from_team_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("migrated_from_team_unique").SetPartialFilterExpression(bson.M{"migrated_from_team_id": bson.M{"$exists": true}})},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure orgs indexes: %w", err)
	}
	if _, err := s.orgMemberships.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "org_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_org_unique")},
		{Keys: bson.D{{Key: "org_id", Value: 1}, {Key: "role", Value: 1}}, Options: options.Index().SetName("org_role")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure org_memberships indexes: %w", err)
	}
	// The unique index below fails its BUILD on a pre-existing duplicate
	// (the pre-check window this closes, or manual edits) — sweep first
	// and name the offenders, so an upgrade fails diagnosably instead of
	// inside the index build.
	cur, err := s.teams.Find(ctx, bson.M{"runner_pool": bson.M{"$exists": true, "$gt": ""}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "slug", Value: 1}, {Key: "runner_pool", Value: 1}}))
	if err != nil {
		return fmt.Errorf("identity: sweep teams for pool duplicates: %w", err)
	}
	var mapped []Team
	if err := cur.All(ctx, &mapped); err != nil {
		return fmt.Errorf("identity: sweep teams for pool duplicates: %w", err)
	}
	if err := duplicatePoolHolders(mapped); err != nil {
		return err
	}
	if _, err := s.teams.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("slug_unique")},
		{Keys: bson.D{{Key: "created_at", Value: -1}}, Options: options.Index().SetName("created_desc")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
		{Keys: bson.D{{Key: "org_id", Value: 1}}, Options: options.Index().SetName("org_id")},
		// D13's one-team-per-pool invariant at the STORE level: the route's
		// check-then-act window (two super-admin writes in the same instant)
		// ends here — only one non-empty runner_pool may exist.
		{Keys: bson.D{{Key: "runner_pool", Value: 1}}, Options: options.Index().SetUnique(true).SetName("runner_pool_unique").SetPartialFilterExpression(bson.M{"runner_pool": bson.M{"$exists": true, "$gt": ""}})},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure teams indexes: %w", err)
	}
	if _, err := s.memberships.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "team_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_team_unique")},
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "role", Value: 1}}, Options: options.Index().SetName("team_role")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure memberships indexes: %w", err)
	}
	if _, err := s.invitations.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique").SetPartialFilterExpression(bson.M{"token_hash": bson.M{"$exists": true}})},
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "email", Value: 1}}, Options: options.Index().SetName("team_email")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("invitations_ttl").SetExpireAfterSeconds(0)},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure invitations indexes: %w", err)
	}
	if _, err := s.oidcLinks.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "provider", Value: 1}, {Key: "provider_user_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("provider_subject_unique")},
		{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("user_id")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure oidc_links indexes: %w", err)
	}
	return nil
}

// findByIDs runs coll.Find({_id: {$in: ids}}) and returns the matching
// documents keyed by id (via idOf). Missing ids are simply absent from
// the map — the bulk analogue of a per-id ErrNotFound — and an empty
// ids slice short-circuits without a query. Shared by the MongoStore
// Get*ByIDs methods.
func findByIDs[T any](ctx context.Context, coll *mongo.Collection, ids []string, idOf func(T) string, findErrMsg, decodeErrMsg string) (map[string]T, error) {
	out := make(map[string]T, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cur, err := coll.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", findErrMsg, err)
	}
	defer cur.Close(ctx)
	var docs []T
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("%s: %w", decodeErrMsg, err)
	}
	for _, d := range docs {
		out[idOf(d)] = d
	}
	return out, nil
}

// ----- Users -----

func (s *MongoStore) CreateUser(ctx context.Context, u User) (User, error) {
	u.Email = NormalizeEmail(u.Email)
	if _, err := s.users.InsertOne(ctx, u); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return User{}, ErrEmailAlreadyTaken
		}
		return User{}, fmt.Errorf("identity: insert user: %w", err)
	}
	return u, nil
}

func (s *MongoStore) GetUser(ctx context.Context, id string) (User, error) {
	return mongoutil.FindOne[User](ctx, s.users, bson.M{"_id": id}, ErrNotFound, "identity: get user")
}

func (s *MongoStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	email = NormalizeEmail(email)
	return mongoutil.FindOne[User](ctx, s.users, bson.M{"email": email}, ErrNotFound, "identity: get user by email")
}

func (s *MongoStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]User, error) {
	return findByIDs(ctx, s.users, ids, func(u User) string { return u.ID },
		"identity: get users by ids", "identity: decode users by ids")
}

func (s *MongoStore) UpdateUser(ctx context.Context, u User) error {
	u.Email = NormalizeEmail(u.Email)
	return mongoutil.ReplaceOneChecked(ctx, s.users, bson.M{"_id": u.ID}, u, ErrEmailAlreadyTaken, ErrNotFound, "identity: update user")
}

// PatchUser $sets only the named fields, under the patch's precondition as a
// filter arm, so a deactivation and a password reset landing together cannot
// undo each other.
func (s *MongoStore) PatchUser(ctx context.Context, id string, p UserPatch) (User, error) {
	filter := userPatchFilter(id, p)
	if p.Empty() {
		// Nothing to write — but the precondition still has to be answered:
		// reading the row back by _id alone would report success on exactly
		// the status the caller refused to act on.
		out, err := mongoutil.FindOne[User](ctx, s.users, filter, ErrNotFound, "identity: patch user")
		if errors.Is(err, ErrNotFound) {
			return User{}, s.userPatchMiss(ctx, id, p)
		}
		return out, err
	}
	var out User
	err := s.users.FindOneAndUpdate(ctx, filter, bson.M{"$set": userPatchSet(p, time.Now().UTC())},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return User{}, s.userPatchMiss(ctx, id, p)
		}
		return User{}, fmt.Errorf("identity: patch user: %w", err)
	}
	return out, nil
}

// userPatchFilter is PatchUser's query: the row by _id, plus — when the patch
// names a precondition — a `$nin` arm on status, which is what makes the
// refusal atomic with the write rather than a check the write races.
//
// Pure, so a test can read what PatchUser ASKS of Mongo without a server;
// whether Mongo honours it is the mongo-conformance job's half.
func userPatchFilter(id string, p UserPatch) bson.M {
	filter := bson.M{"_id": id}
	if len(p.IfNotStatus) > 0 {
		blocked := make([]string, 0, len(p.IfNotStatus))
		for _, st := range p.IfNotStatus {
			blocked = append(blocked, string(st))
		}
		filter["status"] = bson.M{"$nin": blocked}
	}
	if p.IfPasswordHash != nil {
		filter["password_hash"] = *p.IfPasswordHash
	}
	return filter
}

// userPatchSet is PatchUser's `$set` document: one key per field the patch
// names, each spelled with that field's bson tag on User, plus the write's
// own updated_at stamp.
//
// Pure, so a test can apply it through the real bson codec and compare the
// row against the memory twin's: a key that does not match a tag writes a NEW
// document field and leaves the old value standing, silently — the trap
// runPatchSuite's docstring names.
func userPatchSet(p UserPatch, at time.Time) bson.M {
	set := bson.M{"updated_at": at}
	if p.Name != nil {
		set["name"] = *p.Name
	}
	if p.PasswordHash != nil {
		set["password_hash"] = *p.PasswordHash
	}
	if p.Status != nil {
		set["status"] = string(*p.Status)
	}
	if p.IsSuperAdmin != nil {
		set["is_super_admin"] = *p.IsSuperAdmin
	}
	if p.DefaultTeamID != nil {
		set["default_team_id"] = *p.DefaultTeamID
	}
	if p.DefaultOrgID != nil {
		set["default_org_id"] = *p.DefaultOrgID
	}
	if p.GitHubOrgs != nil {
		// Copied into a non-nil slice, like the memory twin: a pointer to a
		// nil slice marshals as BSON null, which decodes back as a nil
		// GitHubOrgs while the memory store answers an empty list.
		set["github_orgs"] = append(make([]string, 0, len(*p.GitHubOrgs)), (*p.GitHubOrgs)...)
	}
	if p.LastLoginAt != nil {
		set["last_login_at"] = *p.LastLoginAt
	}
	if p.Lockout != nil {
		set["failed_logins"] = p.Lockout.FailedLogins
		if p.Lockout.LockedUntil != nil {
			set["locked_until"] = *p.Lockout.LockedUntil
		} else {
			set["locked_until"] = nil
		}
	}
	return set
}

// userPatchMiss tells "no such user" apart from "the precondition refused
// it". The guarded filter cannot say which arm it failed to match, and a
// caller reading a refusal as a missing account would report the wrong thing
// to an operator — so the miss is re-read by _id alone, once, on the failure
// path only. A store error from that read propagates: it is never flattened
// into either verdict.
func (s *MongoStore) userPatchMiss(ctx context.Context, id string, p UserPatch) error {
	if !p.hasPrecondition() {
		return ErrNotFound
	}
	u, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if err := p.precondition(u); err != nil {
		return err
	}
	// The row read back admits the write the filter refused: it moved
	// between the two reads. The refusal stands, under its broadest name.
	return ErrUserStatusChanged
}

func (s *MongoStore) ListUsers(ctx context.Context, f UserFilter) ([]User, error) {
	skip, limit := mongoutil.NormalizePage(f.Offset, f.Limit, 50)
	return mongoutil.FindPageSorted[User](ctx, s.users, userQueryFilter(f.Query), "created_at", skip, limit,
		"identity: list users", "identity: decode users")
}

// userQueryFilter is the Mongo twin of matchesUserQuery — same meaning,
// expressed as a query document, and reading its arms from the same
// normalizeUserQuery so the twins cannot disagree about what the query
// means.
//
// QuoteMeta is load-bearing: Query is operator input reaching a `$regex`,
// and unescaped, `.*` widens the match to every row (measured: 20 001 of
// 20 001, against 0 for the escaped form). The email arm is anchored so the
// unique index on `email` serves it — verified by explain(): the `$or`
// plans as SUBPLAN → OR → [IXSCAN _id_, IXSCAN email_unique], never a
// COLLSCAN, and the bounds stay tight with an escaped metacharacter in the
// pattern.
//
// The email arm is dropped entirely for a prefix MongoDB refuses as a
// pattern — see userQuery.emailArmOff. The id arm survives it.
func userQueryFilter(rawQuery string) bson.M {
	q := normalizeUserQuery(rawQuery)
	if q.matchesAllRow {
		return bson.M{}
	}
	arms := make([]bson.M, 0, 2)
	if !q.emailArmOff {
		arms = append(arms, bson.M{"email": bson.M{"$regex": "^" + regexp.QuoteMeta(q.emailPrefix)}})
	}
	arms = append(arms, bson.M{"_id": q.id})
	return bson.M{"$or": arms}
}

func (s *MongoStore) UserCount(ctx context.Context) (int64, error) {
	n, err := s.users.CountDocuments(ctx, bson.M{})
	if err != nil {
		return 0, fmt.Errorf("identity: count users: %w", err)
	}
	return n, nil
}

// ----- Teams -----

func (s *MongoStore) CreateTeam(ctx context.Context, t Team) (Team, error) {
	if _, err := s.teams.InsertOne(ctx, t); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			// A runner_pool duplicate is the one-team-per-pool invariant
			// speaking at the store level; a slug duplicate is a name
			// collision.
			if t.RunnerPool != "" {
				return Team{}, ErrRunnerPoolHeld
			}
			return Team{}, ErrSlugAlreadyTaken
		}
		return Team{}, fmt.Errorf("identity: insert team: %w", err)
	}
	return t, nil
}

func (s *MongoStore) GetTeam(ctx context.Context, id string) (Team, error) {
	return mongoutil.FindOne[Team](ctx, s.teams, bson.M{"_id": id}, ErrNotFound, "identity: get team")
}

func (s *MongoStore) GetTeamBySlug(ctx context.Context, slug string) (Team, error) {
	return mongoutil.FindOne[Team](ctx, s.teams, bson.M{"slug": slug}, ErrNotFound, "identity: get team by slug")
}

// GetTeamByRunnerPool implements Store.
func (s *MongoStore) GetTeamByRunnerPool(ctx context.Context, pool string) (Team, error) {
	return mongoutil.FindOne[Team](ctx, s.teams, bson.M{"runner_pool": pool}, ErrNotFound, "identity: get team by runner pool")
}

func (s *MongoStore) GetTeamsByIDs(ctx context.Context, ids []string) (map[string]Team, error) {
	return findByIDs(ctx, s.teams, ids, func(t Team) string { return t.ID },
		"identity: get teams by ids", "identity: decode teams by ids")
}

func (s *MongoStore) UpdateTeam(ctx context.Context, t Team) error {
	return mongoutil.ReplaceOneChecked(ctx, s.teams, bson.M{"_id": t.ID}, t, ErrSlugAlreadyTaken, ErrNotFound, "identity: update team")
}

// PatchTeam $sets only the named fields, so a rename and a suspension
// landing together cannot undo each other.
func (s *MongoStore) PatchTeam(ctx context.Context, id string, p TeamPatch) (Team, error) {
	if p.Empty() {
		return s.GetTeam(ctx, id)
	}
	set := bson.M{"updated_at": time.Now().UTC()}
	if p.Name != nil {
		set["name"] = *p.Name
	}
	if p.Slug != nil {
		set["slug"] = *p.Slug
	}
	if p.Status != nil {
		set["status"] = string(*p.Status)
		if *p.Status == TeamStatusSuspended {
			set["suspended_at"] = time.Now().UTC()
			set["suspended_by"] = p.SuspendedBy
			set["suspend_reason"] = p.SuspendReason
		} else {
			set["suspended_at"] = nil
			set["suspended_by"] = ""
			set["suspend_reason"] = ""
		}
	}
	if p.RunnerPool != nil {
		set["runner_pool"] = *p.RunnerPool
	}
	if p.LLMFallback != nil {
		set["llm_fallback"] = *p.LLMFallback
	}
	if p.OrgID != nil {
		set["org_id"] = *p.OrgID
	}
	if p.MaxConcurrentRuns != nil {
		set["max_concurrent_runs"] = *p.MaxConcurrentRuns
	}
	if p.LaunchRatePerMin != nil {
		set["launch_rate_per_min"] = *p.LaunchRatePerMin
	}
	var out Team
	err := s.teams.FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Team{}, ErrNotFound
		}
		if mongoutil.IsDuplicateKey(err) {
			// A runner_pool duplicate is the one-team-per-pool invariant
			// speaking at the store level (the partial unique index); a
			// slug duplicate is a rename collision.
			if p.RunnerPool != nil && *p.RunnerPool != "" {
				return Team{}, ErrRunnerPoolHeld
			}
			return Team{}, ErrSlugAlreadyTaken
		}
		return Team{}, fmt.Errorf("identity: patch team: %w", err)
	}
	return out, nil
}

func (s *MongoStore) DeleteTeam(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.teams, bson.M{"_id": id}, ErrNotFound, "identity: delete team")
}

func (s *MongoStore) ListTeams(ctx context.Context, page Page) ([]Team, error) {
	skip, limit := mongoutil.NormalizePage(page.Offset, page.Limit, 50)
	return mongoutil.FindPageSorted[Team](ctx, s.teams, bson.M{}, "created_at", skip, limit,
		"identity: list teams", "identity: decode teams")
}

func (s *MongoStore) ListTeamsByOrg(ctx context.Context, orgID string) ([]Team, error) {
	return mongoutil.FindAllSorted[Team](ctx, s.teams, bson.M{"org_id": orgID}, "created_at",
		"identity: list teams by org", "identity: decode teams")
}

// ----- Orgs -----

func (s *MongoStore) CreateOrg(ctx context.Context, o Org) (Org, error) {
	if _, err := s.orgs.InsertOne(ctx, o); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return Org{}, ErrOrgSlugAlreadyTaken
		}
		return Org{}, fmt.Errorf("identity: insert org: %w", err)
	}
	return o, nil
}

func (s *MongoStore) GetOrg(ctx context.Context, id string) (Org, error) {
	return mongoutil.FindOne[Org](ctx, s.orgs, bson.M{"_id": id}, ErrNotFound, "identity: get org")
}

func (s *MongoStore) GetOrgBySlug(ctx context.Context, slug string) (Org, error) {
	return mongoutil.FindOne[Org](ctx, s.orgs, bson.M{"slug": slug}, ErrNotFound, "identity: get org by slug")
}

func (s *MongoStore) GetOrgsByIDs(ctx context.Context, ids []string) (map[string]Org, error) {
	return findByIDs(ctx, s.orgs, ids, func(o Org) string { return o.ID },
		"identity: get orgs by ids", "identity: decode orgs by ids")
}

func (s *MongoStore) UpdateOrg(ctx context.Context, o Org) error {
	return mongoutil.ReplaceOneChecked(ctx, s.orgs, bson.M{"_id": o.ID}, o, ErrOrgSlugAlreadyTaken, ErrNotFound, "identity: update org")
}

// PatchOrg is PatchTeam's twin: the org settings, the credential audience
// and the super-admin plan fields are three independent editors of one
// document, and a whole-document replace made each one able to revert the
// others.
func (s *MongoStore) PatchOrg(ctx context.Context, id string, p OrgPatch) (Org, error) {
	if p.Empty() {
		return s.GetOrg(ctx, id)
	}
	set := bson.M{"updated_at": time.Now().UTC()}
	if p.Name != nil {
		set["name"] = *p.Name
	}
	if p.Slug != nil {
		set["slug"] = *p.Slug
	}
	if p.RequireProvisionApproval != nil {
		set["require_provision_approval"] = *p.RequireProvisionApproval
	}
	if p.ProvisionApprovalScope != nil {
		set["provision_approval_scope"] = string(*p.ProvisionApprovalScope)
	}
	if p.CredentialAudience != nil {
		set["credential_audience"] = *p.CredentialAudience
	}
	var out Org
	err := s.orgs.FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Org{}, ErrNotFound
		}
		if mongoutil.IsDuplicateKey(err) {
			return Org{}, ErrOrgSlugAlreadyTaken
		}
		return Org{}, fmt.Errorf("identity: patch org: %w", err)
	}
	return out, nil
}

func (s *MongoStore) DeleteOrg(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.orgs, bson.M{"_id": id}, ErrNotFound, "identity: delete org")
}

func (s *MongoStore) ListOrgsPendingPurge(ctx context.Context, before time.Time) ([]Org, error) {
	cur, err := s.orgs.Find(ctx, bson.M{
		"status":      string(TeamStatusPendingDeletion),
		"purge_after": bson.M{"$lte": before},
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list orgs pending purge: %w", err)
	}
	defer cur.Close(ctx)
	var out []Org
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode orgs pending purge: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListOrgs(ctx context.Context, page Page) ([]Org, error) {
	skip, limit := mongoutil.NormalizePage(page.Offset, page.Limit, 50)
	return mongoutil.FindPageSorted[Org](ctx, s.orgs, bson.M{}, "created_at", skip, limit,
		"identity: list orgs", "identity: decode orgs")
}

// ----- Org memberships -----

func (s *MongoStore) UpsertOrgMembership(ctx context.Context, m OrgMembership) error {
	if !m.Role.Valid() {
		return ErrInvalidRole
	}
	filter := bson.M{"user_id": m.UserID, "org_id": m.OrgID}
	update := bson.M{"$set": m}
	_, err := s.orgMemberships.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("identity: upsert org membership: %w", err)
	}
	return nil
}

func (s *MongoStore) GetOrgMembership(ctx context.Context, userID, orgID string) (OrgMembership, error) {
	return mongoutil.FindOne[OrgMembership](ctx, s.orgMemberships, bson.M{"user_id": userID, "org_id": orgID}, ErrNotFound, "identity: get org membership")
}

func (s *MongoStore) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	_, err := s.orgMemberships.DeleteOne(ctx, bson.M{"user_id": userID, "org_id": orgID})
	if err != nil {
		return fmt.Errorf("identity: delete org membership: %w", err)
	}
	return nil
}

func (s *MongoStore) ListOrgMembershipsByUser(ctx context.Context, userID string) ([]OrgMembership, error) {
	return mongoutil.FindAllSorted[OrgMembership](ctx, s.orgMemberships, bson.M{"user_id": userID}, "joined_at",
		"identity: list org memberships by user", "identity: decode org memberships")
}

func (s *MongoStore) ListOrgMembershipsByOrg(ctx context.Context, orgID string) ([]OrgMembership, error) {
	return mongoutil.FindAllSorted[OrgMembership](ctx, s.orgMemberships, bson.M{"org_id": orgID}, "joined_at",
		"identity: list org memberships by org", "identity: decode org memberships")
}

// ----- Memberships -----

func (s *MongoStore) UpsertMembership(ctx context.Context, m Membership) error {
	if !m.Role.Valid() {
		return ErrInvalidRole
	}
	filter := bson.M{"user_id": m.UserID, "team_id": m.TeamID}
	update := bson.M{"$set": m}
	_, err := s.memberships.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("identity: upsert membership: %w", err)
	}
	return nil
}

func (s *MongoStore) GetMembership(ctx context.Context, userID, teamID string) (Membership, error) {
	return mongoutil.FindOne[Membership](ctx, s.memberships, bson.M{"user_id": userID, "team_id": teamID}, ErrNotFound, "identity: get membership")
}

func (s *MongoStore) DeleteMembership(ctx context.Context, userID, teamID string) error {
	_, err := s.memberships.DeleteOne(ctx, bson.M{"user_id": userID, "team_id": teamID})
	if err != nil {
		return fmt.Errorf("identity: delete membership: %w", err)
	}
	return nil
}

func (s *MongoStore) ListMembershipsByUser(ctx context.Context, userID string) ([]Membership, error) {
	return mongoutil.FindAllSorted[Membership](ctx, s.memberships, bson.M{"user_id": userID}, "joined_at",
		"identity: list memberships by user", "identity: decode memberships")
}

func (s *MongoStore) ListMembershipsByTeam(ctx context.Context, teamID string) ([]Membership, error) {
	return mongoutil.FindAllSorted[Membership](ctx, s.memberships, bson.M{"team_id": teamID}, "joined_at",
		"identity: list memberships by team", "identity: decode memberships")
}

// ----- Invitations -----

func (s *MongoStore) CreateInvitation(ctx context.Context, inv Invitation) error {
	_, err := s.invitations.InsertOne(ctx, inv)
	if err != nil {
		return fmt.Errorf("identity: insert invitation: %w", err)
	}
	return nil
}

func (s *MongoStore) GetInvitation(ctx context.Context, id string) (Invitation, error) {
	return mongoutil.FindOne[Invitation](ctx, s.invitations, bson.M{"_id": id}, ErrNotFound, "identity: get invitation")
}

func (s *MongoStore) GetInvitationByTokenHash(ctx context.Context, tokenHash string) (Invitation, error) {
	return mongoutil.FindOne[Invitation](ctx, s.invitations, bson.M{"token_hash": tokenHash}, ErrNotFound, "identity: get invitation by token")
}

func (s *MongoStore) UpdateInvitation(ctx context.Context, inv Invitation) error {
	return mongoutil.ReplaceOneChecked(ctx, s.invitations, bson.M{"_id": inv.ID}, inv, nil, ErrNotFound, "identity: update invitation")
}

func (s *MongoStore) DeleteInvitation(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.invitations, bson.M{"_id": id}, ErrNotFound, "identity: delete invitation")
}

func (s *MongoStore) ListInvitationsByTeam(ctx context.Context, teamID string) ([]Invitation, error) {
	return mongoutil.FindAllSorted[Invitation](ctx, s.invitations, bson.M{"team_id": teamID}, "created_at",
		"identity: list invitations", "identity: decode invitations")
}

// ----- OIDC links -----

func (s *MongoStore) UpsertOIDCLink(ctx context.Context, link OIDCLink) error {
	filter := bson.M{"provider": link.Provider, "provider_user_id": link.ProviderUserID}
	if link.CreatedAt.IsZero() {
		link.CreatedAt = time.Now().UTC()
	}
	update := bson.M{"$set": link}
	_, err := s.oidcLinks.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("identity: upsert oidc link: %w", err)
	}
	return nil
}

func (s *MongoStore) GetOIDCLink(ctx context.Context, provider, providerUserID string) (OIDCLink, error) {
	return mongoutil.FindOne[OIDCLink](ctx, s.oidcLinks, bson.M{"provider": provider, "provider_user_id": providerUserID}, ErrNotFound, "identity: get oidc link")
}

func (s *MongoStore) ListOIDCLinksByUser(ctx context.Context, userID string) ([]OIDCLink, error) {
	return mongoutil.FindAllSorted[OIDCLink](ctx, s.oidcLinks, bson.M{"user_id": userID}, "provider",
		"identity: list oidc links", "identity: decode oidc links")
}

func (s *MongoStore) DeleteOIDCLink(ctx context.Context, provider, providerUserID string) error {
	return mongoutil.DeleteOneChecked(ctx, s.oidcLinks, bson.M{"provider": provider, "provider_user_id": providerUserID}, ErrNotFound, "identity: delete oidc link")
}

// DeleteSessionOIDCLinks removes the SSO identity links a signed-in session
// wrote (SessionLineage set), all of them or one lineage's — with the
// keep-one rule the memory twin runs: an account with no password of which
// EVERY link matches keeps its OLDEST link, so the cleanup cannot lock its
// owner out of an account that signs in through SSO only.
func (s *MongoStore) DeleteSessionOIDCLinks(ctx context.Context, userID, lineageID string) (int64, error) {
	if userID == "" {
		return 0, errors.New("identity: delete session oidc links: no user id")
	}
	filter := bson.M{"user_id": userID, "session_lineage": bson.M{"$exists": true, "$ne": ""}}
	if lineageID != "" {
		filter = bson.M{"user_id": userID, "session_lineage": lineageID}
	}
	candidates, err := s.oidcLinks.CountDocuments(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("identity: count the session oidc links of user %s: %w", userID, err)
	}
	if candidates == 0 {
		return 0, nil
	}
	deleteFilter := filter
	total, err := s.oidcLinks.CountDocuments(ctx, bson.M{"user_id": userID})
	if err != nil {
		return 0, fmt.Errorf("identity: count the oidc links of user %s: %w", userID, err)
	}
	if total == candidates {
		// Every link the removal matches: the account may be left with none.
		if u, err := mongoutil.FindOne[User](ctx, s.users, bson.M{"_id": userID}, ErrNotFound, "identity: get user"); err == nil && u.PasswordHash == "" {
			kept, err := s.oldestOIDCLink(ctx, filter)
			if err != nil {
				return 0, err
			}
			if kept != nil {
				// The candidates' own scope, minus the kept link: the account
				// is in the filter already, and the kept link is named by its
				// own provider and subject.
				deleteFilter = bson.M{
					"user_id":         userID,
					"session_lineage": filter["session_lineage"],
					"$nor":            []bson.M{{"provider": kept.Provider, "provider_user_id": kept.ProviderUserID}},
				}
			}
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return 0, fmt.Errorf("identity: read the account of the oidc links of user %s: %w", userID, err)
		}
	}
	res, err := s.oidcLinks.DeleteMany(ctx, deleteFilter)
	if err != nil {
		return 0, fmt.Errorf("identity: delete the session oidc links of user %s: %w", userID, err)
	}
	return res.DeletedCount, nil
}

// oldestOIDCLink answers the oldest link matching filter (created_at, then
// provider and subject for ties), or nil.
func (s *MongoStore) oldestOIDCLink(ctx context.Context, filter bson.M) (*OIDCLink, error) {
	link, err := mongoutil.FindOne[OIDCLink](ctx, s.oidcLinks, filter, ErrNotFound,
		"identity: find the oldest session oidc link",
		options.FindOne().SetSort(bson.D{
			{Key: "created_at", Value: 1},
			{Key: "provider", Value: 1},
			{Key: "provider_user_id", Value: 1},
		}))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

// duplicatePoolHolders reports the teams that share a non-empty
// runner_pool — the pre-flight the unique index's build would otherwise
// fail on undiagnosed.
func duplicatePoolHolders(mapped []Team) error {
	holders := map[string][]string{}
	for _, t := range mapped {
		if t.RunnerPool == "" {
			continue
		}
		holders[t.RunnerPool] = append(holders[t.RunnerPool], t.ID+"/"+t.Slug)
	}
	pools := make([]string, 0, len(holders))
	for pool, ids := range holders {
		if len(ids) > 1 {
			pools = append(pools, fmt.Sprintf("%q held by %s", pool, strings.Join(ids, ", ")))
		}
	}
	sort.Strings(pools)
	if len(pools) > 0 {
		return fmt.Errorf("identity: the one-team-per-pool invariant is violated — unmap all but one team per pool before upgrading: %s", strings.Join(pools, "; "))
	}
	return nil
}
