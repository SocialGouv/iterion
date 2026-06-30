// Package marketplace is the hosted bot registry that sits on top of
// pkg/botinstall. A repository "publishes" a bot by holding a bundle
// directory (main.bot + manifest.yaml) and optionally an
// `iterion-bots.yaml` index — see pkg/botinstall. The marketplace adds
// a curated index over those repos: an author submits a repo URL, the
// server validates it via botinstall.Inspect, persists an Entry, and
// users browse + install by slug. Install resolves the entry's repo URL
// and reuses botinstall.Install — the marketplace is metadata only; the
// actual install path is unchanged from Phase A.
package marketplace

import (
	"context"
	"errors"
)

// Scope controls who may browse an entry once it is approved. The zero
// value ("") reads as ScopePublic via EffectiveScope so legacy flat
// entries (written before scoping existed) stay world-visible.
type Scope string

const (
	ScopePublic   Scope = "public"   // anyone, including unauthenticated, may browse
	ScopeInstance Scope = "instance" // any authenticated user of this instance
	ScopeOrg      Scope = "org"      // only members of OrgID
)

// Status is the moderation state of an entry. The zero value ("") reads
// as StatusApproved via EffectiveStatus — local single-tenant entries
// and pre-moderation registries are implicitly approved.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

// Kind distinguishes a marketplace entry's artifact type. The zero value
// ("") reads as KindBot via EffectiveKind so every legacy entry (written
// before plugins shared the registry) stays a bot.
type Kind string

const (
	KindBot    Kind = "bot"    // a .bot/.botz workflow bundle (the original entry type)
	KindPlugin Kind = "plugin" // a plugin.yaml package (rewriter / mcp / skill / lifecycle)
)

// Source records how an entry's bundle is sourced. The zero value ("")
// reads as SourceGit via EffectiveSource.
type Source string

const (
	SourceGit     Source = "git"     // cloned from RepoURL (default)
	SourceUpload  Source = "upload"  // uploaded .botz (see BundleRef)
	SourceBuiltin Source = "builtin" // seeded from the host's bots/ catalog
)

// ErrStatusConflict is returned by SetStatus when the CAS guard fails
// (the stored status no longer matches the expected one) — surfaced as
// HTTP 409 by the moderation handlers.
var ErrStatusConflict = errors.New("marketplace: status transition conflict")

// Entry is one bot listing in the marketplace registry.
//
// Timestamps (CreatedAt, UpdatedAt) are RFC3339 strings supplied by the
// caller (Server handlers) rather than minted inside the store. This
// keeps the store deterministic (essential for testing) and is the same
// convention used elsewhere in the codebase that needs reproducible
// time stamps.
type Entry struct {
	// Slug is the registry-unique id, derived from the manifest name
	// via botregistry.NormalizeName. Used as the URL segment in
	// `/api/v1/marketplace/bots/{slug}` and as the Mongo _id.
	Slug string `json:"slug" bson:"_id"`

	// Kind is the entry's artifact type (bot | plugin). Empty reads as
	// KindBot via EffectiveKind (legacy entries).
	Kind Kind `json:"kind,omitempty" bson:"kind,omitempty"`

	// Categories are a plugin entry's contribution kinds (rewriter / mcp /
	// skill / command / agent / hook / lifecycle), used to group it in the
	// marketplace by plugin type. Empty for bots (grouped under "bot"). For a
	// plugin it is the plugin manifest's Kinds().
	Categories []string `json:"categories,omitempty" bson:"categories,omitempty"`

	// Name is the bundle's technical id (manifest.name).
	Name string `json:"name" bson:"name"`

	// DisplayName is the operator-facing label (manifest.display_name).
	DisplayName string `json:"display_name,omitempty" bson:"display_name,omitempty"`

	// Description is the one-line summary shown on the card.
	Description string `json:"description,omitempty" bson:"description,omitempty"`

	// Author is the free-form attribution (manifest.author).
	Author string `json:"author,omitempty" bson:"author,omitempty"`

	// Tags are operator-supplied at submit time (e.g. "review",
	// "kanban") — distinct from manifest.triggers, which the engine
	// uses to match a bot to a ticket. Tags drive marketplace browse
	// filtering and have no engine semantics.
	Tags []string `json:"tags,omitempty" bson:"tags,omitempty"`

	// RepoURL, Ref and Subpath are the install coordinates persisted
	// from the submit form. The Install endpoint forwards them to
	// botinstall.Install verbatim.
	RepoURL string `json:"repo_url" bson:"repo_url"`
	Ref     string `json:"ref,omitempty" bson:"ref,omitempty"`
	Subpath string `json:"subpath,omitempty" bson:"subpath,omitempty"`

	// Version mirrors manifest.version at index time. The registry is
	// a snapshot — operators re-submit to refresh.
	Version string `json:"version,omitempty" bson:"version,omitempty"`

	// README is the bundle's README.md content (capped — see
	// botinstall.Inspect). Shown in the detail panel.
	README string `json:"readme,omitempty" bson:"readme,omitempty"`

	// Presets mirrors the bundle's file-based presets metadata. Only
	// the registry-relevant slice is kept (no prompt body / vars map);
	// callers that need the full preset bias install the bot.
	Presets []EntryPreset `json:"presets,omitempty" bson:"presets,omitempty"`

	// Installs is the rough install count, bumped each time the
	// install endpoint succeeds. Approximate (no per-user dedup) —
	// good enough to surface "popular" bots.
	Installs int `json:"installs" bson:"installs"`

	// CreatedAt / UpdatedAt are RFC3339 timestamps stamped by the
	// caller.
	CreatedAt string `json:"created_at,omitempty" bson:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty" bson:"updated_at,omitempty"`

	// --- Multi-scope / moderation plumbing (cloud) ---
	//
	// All of these are omitempty and read through Effective* helpers so a
	// legacy flat entry (none of them set) behaves exactly as before:
	// public, approved, git-sourced. Local single-tenant mode leaves them
	// empty; only cloud handlers populate them.

	// Scope controls browse visibility once approved (see EffectiveScope).
	Scope Scope `json:"scope,omitempty" bson:"scope,omitempty"`

	// OrgID owns the entry when Scope == ScopeOrg (the submitting team).
	OrgID string `json:"org_id,omitempty" bson:"org_id,omitempty"`

	// Status is the moderation state (see EffectiveStatus).
	Status Status `json:"status,omitempty" bson:"status,omitempty"`

	// Source records how the bundle is sourced (see EffectiveSource).
	Source Source `json:"source,omitempty" bson:"source,omitempty"`

	// BundleRef points at an uploaded bundle when Source == SourceUpload.
	// Reserved for a future cloud bundle-hosting backend; unused while
	// uploads are a local-install convenience.
	BundleRef string `json:"bundle_ref,omitempty" bson:"bundle_ref,omitempty"`

	// SubmittedBy is the user id of the submitter (cloud). The submitter
	// always sees their own entry regardless of moderation status.
	SubmittedBy string `json:"submitted_by,omitempty" bson:"submitted_by,omitempty"`

	// ReviewedBy / ReviewedAt / RejectReason record the moderation
	// decision. RejectReason is shown back to the submitter.
	ReviewedBy   string `json:"reviewed_by,omitempty" bson:"reviewed_by,omitempty"`
	ReviewedAt   string `json:"reviewed_at,omitempty" bson:"reviewed_at,omitempty"`
	RejectReason string `json:"reject_reason,omitempty" bson:"reject_reason,omitempty"`
}

// EffectiveStatus resolves the moderation status, treating the empty
// zero value as approved (legacy / local entries).
func EffectiveStatus(e Entry) Status {
	if e.Status == "" {
		return StatusApproved
	}
	return e.Status
}

// EffectiveScope resolves the visibility scope, treating the empty zero
// value as public.
func EffectiveScope(e Entry) Scope {
	if e.Scope == "" {
		return ScopePublic
	}
	return e.Scope
}

// EffectiveKind resolves the artifact kind, treating the empty zero value
// as a bot (legacy entries written before plugins shared the registry).
func EffectiveKind(e Entry) Kind {
	if e.Kind == "" {
		return KindBot
	}
	return e.Kind
}

// EffectiveSource resolves the bundle source, treating the empty zero
// value as a git clone.
func EffectiveSource(e Entry) Source {
	if e.Source == "" {
		return SourceGit
	}
	return e.Source
}

// Visible reports whether viewer v may see entry e in a browse listing.
// When v.Enforce is false (local single-tenant mode) every entry is
// visible to the sole operator. Otherwise the rule is:
//   - the submitter always sees their own entry (any status);
//   - everyone else sees only approved entries within their scope reach
//     (public → anyone, instance → any authenticated user, org → members
//     of OrgID; a super-admin sees every approved entry).
//
// Moderation queues (pending/rejected for non-owners) are a separate
// path — see ListForModeration — never surfaced through Visible.
func Visible(e Entry, v ViewerContext) bool {
	if !v.Enforce {
		return true
	}
	if v.Authenticated && v.UserID != "" && e.SubmittedBy == v.UserID {
		return true
	}
	if EffectiveStatus(e) != StatusApproved {
		return false
	}
	switch EffectiveScope(e) {
	case ScopePublic:
		return true
	case ScopeInstance:
		return v.Authenticated
	case ScopeOrg:
		if v.IsSuperAdmin {
			return true
		}
		for _, o := range v.OrgIDs {
			if o == e.OrgID {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// ViewerContext describes the principal browsing the registry, used to
// filter entries by scope + moderation status. The zero value (Enforce
// false) means "local single-tenant operator" — no filtering at all.
type ViewerContext struct {
	// Enforce turns scope/status filtering on. Cloud handlers set it
	// true; local mode leaves it false so the sole operator sees
	// everything.
	Enforce bool
	// Authenticated is true when a verified identity backs the request.
	Authenticated bool
	// UserID is the authenticated user's id (owner-sees-own-entry rule).
	UserID string
	// OrgIDs are the teams the viewer belongs to (org-scope reach).
	OrgIDs []string
	// IsSuperAdmin grants platform-wide visibility.
	IsSuperAdmin bool
}

// Review carries the moderation decision metadata stamped onto an entry
// by SetStatus. By/At are RFC3339-stamped by the caller; Reason is the
// rejection explanation shown to the submitter.
type Review struct {
	By     string
	At     string
	Reason string
}

// ModerationQuery selects entries for a moderation queue. Statuses
// defaults to {StatusPending} when empty. When All is false the result
// is restricted to OrgIDs (org admins); All=true returns every matching
// entry (super-admin).
type ModerationQuery struct {
	Statuses []Status
	OrgIDs   []string
	All      bool
}

// EntryPreset is the slim, registry-facing slice of bundle.PresetSpec.
// Vars + prompt body are intentionally omitted — they're only useful at
// run time after install.
type EntryPreset struct {
	Name        string   `json:"name" bson:"name"`
	DisplayName string   `json:"display_name,omitempty" bson:"display_name,omitempty"`
	Description string   `json:"description,omitempty" bson:"description,omitempty"`
	Skills      []string `json:"skills,omitempty" bson:"skills,omitempty"`
}

// Query filters a List call. Zero-value fields don't filter.
type Query struct {
	// Text is a case-insensitive substring matched against Slug,
	// Name, DisplayName, Description, Author and Tags.
	Text string
	// Tag, when set, requires an exact (case-insensitive) match in
	// the entry's Tags.
	Tag string
	// Kind, when set, restricts the listing to entries of that artifact
	// kind (bot | plugin), compared via EffectiveKind. Empty = any.
	Kind Kind
	// Viewer scopes the listing to what the requesting principal may
	// see (see Visible). The zero value (Enforce false) returns every
	// entry — the local single-tenant default.
	Viewer ViewerContext
}

// Store is the marketplace persistence interface. Two implementations
// land alongside this file: a JSON-file store for self-host / local
// mode (jsonstore.go) and a Mongo store for cloud mode (mongostore.go).
type Store interface {
	List(ctx context.Context, q Query) ([]Entry, error)
	Get(ctx context.Context, slug string) (*Entry, bool, error)
	Upsert(ctx context.Context, e Entry) error
	IncrementInstalls(ctx context.Context, slug string) error

	// SetStatus transitions an entry's moderation status. When expect is
	// non-empty it is a CAS guard: the update applies only when the
	// stored EffectiveStatus equals expect, else ErrStatusConflict. The
	// review metadata (reviewer/time/reason) is stamped onto the entry.
	SetStatus(ctx context.Context, slug string, expect, next Status, review Review) error

	// ListForModeration returns entries awaiting or holding a moderation
	// decision, scoped per ModerationQuery.
	ListForModeration(ctx context.Context, q ModerationQuery) ([]Entry, error)

	// Delete removes an entry by slug. A missing slug is not an error.
	Delete(ctx context.Context, slug string) error
}
