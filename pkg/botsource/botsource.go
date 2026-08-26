// Package botsource persists TEAM-AUTHORED bot bundles: the writable,
// tenant-scoped counterpart to the read-only catalog baked into a runner image.
//
// Why it exists. In cloud mode the bot catalog is baked into the pod image at
// a read-only path, and the studio editor's save path is filesystem-only — so
// cloud studios can view catalog bots but cannot create or edit their own. A
// BotSource moves the authority off the pod's ephemeral filesystem into a
// durable, team-scoped Mongo store (memory-backed in tests/local), exactly like
// pkg/pluginsource does for plugins — but it stores the bundle CONTENT (a
// multi-file map: main.bot + manifest.yaml + skills/*, prompts/, …) rather than
// a git binding, because the content is authored in the UI, not fetched.
//
// Editability is two-tier: baked catalog bots stay read-only; a tenant can fork
// one into the store (Origin = "forked:<catalog-id>") and then edit it, or
// author a new one from scratch.
package botsource

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// MainBotFile is the required workflow entry of every bundle — mirrors
// bundle.MainBotFile so a source cannot persist a bundle the loader would reject.
const MainBotFile = bundle.MainBotFile

// PlatformTenantID is the sentinel tenant under which the DEPLOYMENT's own
// bot overrides are stored — the DB-backed form of the catalog baked into
// the server/runner images, so iterating on a native bot is one API/CLI
// call instead of an image build + rollout. Platform overrides are ordinary
// BotSource rows with TenantID = PlatformTenantID, written and read under
// store.WithTenant(ctx, PlatformTenantID): same collection, same
// validation, zero schema change — the botsource counterpart of
// secrets.PlatformTenantID. The ':' suffix keeps the literal outside the
// identity space of real team ids (mirrors the secrets sentinel family).
//
// Resolution precedence everywhere a bot id resolves: team botsource →
// platform botsource → baked catalog FS. Super-admin only: a platform
// override runs across ALL tenants, the same trust level as the image.
const PlatformTenantID = "platform:"

// Bundle size limits enforced by Validate. A BotSource is one Mongo
// document; unbounded content would silently approach the 16 MB BSON cap
// and start failing writes with an opaque driver error. The limits keep a
// row an order of magnitude below that wall and are far above every
// shipped catalog bundle (largest ≈ 700 KB).
const (
	// MaxBundleBytes caps the sum of all file paths + contents.
	MaxBundleBytes = 6 << 20 // 6 MiB
	// MaxBundleFiles caps the file-map entry count.
	MaxBundleFiles = 512
)

// BotSource is a team-authored bot bundle held as a file map.
type BotSource struct {
	ID       string `bson:"_id" json:"id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	// Slug is the bot's technical name — its identity within the team and the
	// directory it materializes into at launch. Unique per team.
	Slug string `bson:"slug" json:"slug"`
	// Files maps a bundle-relative path to its content. It always contains
	// main.bot; it may also carry manifest.yaml, skills/*.md, prompts/, etc.
	Files map[string]string `bson:"files" json:"files"`
	// Origin records provenance: "tenant" for a from-scratch bot, or
	// "forked:<catalog-id>" for a copy of a baked catalog bot.
	Origin string `bson:"origin,omitempty" json:"origin,omitempty"`

	CreatedBy string `bson:"created_by,omitempty" json:"created_by,omitempty"`
	UpdatedBy string `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
	// Version increments on every successful write. It is the optimistic-
	// concurrency token: a PUT may carry an if-match version to reject a write
	// that would clobber a concurrent editor.
	Version int `bson:"version" json:"version"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Store persists bot sources. Mongo-backed in cloud, memory in tests/local.
type Store interface {
	Create(ctx context.Context, s BotSource) (BotSource, error)
	Get(ctx context.Context, id string) (BotSource, error)
	GetBySlug(ctx context.Context, tenantID, slug string) (BotSource, error)
	Update(ctx context.Context, s BotSource) (BotSource, error)
	Delete(ctx context.Context, id string) error
	ListByTenant(ctx context.Context, tenantID string) ([]BotSource, error)
}

var (
	ErrNotFound      = errors.New("botsource: not found")
	ErrTenantMissing = errors.New("botsource: store called without tenant context")
	ErrSlugConflict  = errors.New("botsource: a bot with this slug already exists for the team")
	// ErrVersionConflict is returned when an if-match version does not match the
	// stored version — a concurrent editor wrote in between.
	ErrVersionConflict = errors.New("botsource: version conflict — the bot was modified by another write")
)

// Validate checks a source's STRUCTURE before it is persisted. It is
// deliberately pure (no filesystem, no compile) — compilability is checked at
// the route layer where the full bundle context (prompt merge) is available and
// an HTTP 400 is the right response. Errors are explicit: a malformed source
// must fail at write time, not silently break at launch.
func (s *BotSource) Validate() error {
	if s.TenantID == "" {
		return ErrTenantMissing
	}
	if err := ValidSlug(s.Slug); err != nil {
		return err
	}
	if len(s.Files) == 0 {
		return fmt.Errorf("botsource: files is empty (main.bot is required)")
	}
	if len(s.Files) > MaxBundleFiles {
		return fmt.Errorf("botsource: bundle has %d files, over the %d-file limit", len(s.Files), MaxBundleFiles)
	}
	if strings.TrimSpace(s.Files[MainBotFile]) == "" {
		return fmt.Errorf("botsource: %s is required and must not be empty", MainBotFile)
	}
	total := 0
	for key, content := range s.Files {
		if err := safeBundlePath(key); err != nil {
			return err
		}
		total += len(key) + len(content)
	}
	if total > MaxBundleBytes {
		return fmt.Errorf("botsource: bundle is %d bytes, over the %d-byte limit", total, MaxBundleBytes)
	}
	if body, ok := s.Files[bundle.ManifestFile]; ok {
		if _, err := bundle.DecodeManifest([]byte(body), bundle.ManifestFile); err != nil {
			return fmt.Errorf("botsource: %s: %w", bundle.ManifestFile, err)
		}
	}
	if body, ok := s.Files[bundle.ManifestFileAlt]; ok {
		if _, err := bundle.DecodeManifest([]byte(body), bundle.ManifestFileAlt); err != nil {
			return fmt.Errorf("botsource: %s: %w", bundle.ManifestFileAlt, err)
		}
	}
	return nil
}

// ValidSlug mirrors the catalog naming rule: lowercase alphanumerics plus '-'
// and '_', no path separators (the slug becomes a directory at launch).
func ValidSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("botsource: slug is required")
	}
	if len(slug) > 64 {
		return fmt.Errorf("botsource: slug %q exceeds 64 characters", slug)
	}
	for _, c := range slug {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("botsource: slug %q must be lowercase alphanumeric, '-' or '_'", slug)
		}
	}
	return nil
}

// safeBundlePath rejects any file key that could escape the bundle directory
// when materialized to disk — the anti-traversal boundary. Content is untrusted
// operator input that becomes files in a run workspace, so a "../" or absolute
// key must never persist.
func safeBundlePath(key string) error {
	if key == "" {
		return fmt.Errorf("botsource: empty file path")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("botsource: file path %q must be relative", key)
	}
	if strings.ContainsAny(key, "\\") {
		return fmt.Errorf("botsource: file path %q must use '/' separators", key)
	}
	cleaned := path.Clean(key)
	if cleaned != key {
		return fmt.Errorf("botsource: file path %q is not normalized (want %q)", key, cleaned)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("botsource: file path %q escapes the bundle", key)
	}
	// A .git/ tree in a materialized bundle would shadow the run worktree's git
	// state — never let one persist.
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return fmt.Errorf("botsource: file path %q is not allowed", key)
	}
	return nil
}
