// Package pluginsource persists ORG-PRIVATE plugin bindings: "this team's
// runs get the plugin living in this git repository".
//
// Why it exists. ADR-079 made an enabled plugin's skills reach a cloud runner
// pod, but it resolves them from the LAUNCHING instance's iterion home — and a
// cloud server pod's home is ephemeral too. An operator who installs a private
// plugin there loses it on the next restart, silently: the mirror is
// best-effort, so runs simply proceed without the skill and produce a
// plausible-looking wrong result. There is also no way to scope a plugin to one
// org: enablement is a global per-instance toggle.
//
// A PluginSource fixes both by moving the AUTHORITY off the pod's filesystem:
// the durable record (in Mongo, cloud state) says which git repo holds the
// plugin and which stored secret reads it. The checkout is only ever a
// re-derivable cache. Team-scoped, so each org brings its own private plugins.
//
// The credential is referenced, never inlined: SecretID points at a
// GenericSecret (a PAT or deploy key) the fetcher consumes without the value
// passing through this package.
package pluginsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PluginSource binds a team to a plugin hosted in a git repository.
type PluginSource struct {
	ID       string `bson:"_id" json:"id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	// Name is the plugin name as it will appear in the registry. It must
	// match the plugin.yaml `name` when the repo carries a manifest; for a
	// bare skills/ repo it names the synthesized skills-only plugin.
	Name string `bson:"name" json:"name"`
	// GitURL is the clone URL (https or ssh, per the credential kind).
	GitURL string `bson:"git_url" json:"git_url"`
	// Ref is what to check out. PIN A TAG OR SHA in production: a moving
	// branch turns every launch into a network round-trip AND makes the
	// skill change under the operator's feet, which is the silent-drift
	// failure this whole area exists to prevent. A pinned ref makes the
	// cache immutable and updates an explicit, auditable act.
	Ref string `bson:"ref" json:"ref"`
	// SecretID references the GenericSecret holding the read credential
	// (PAT or deploy key). Empty for a public repository.
	SecretID string `bson:"secret_id,omitempty" json:"secret_id,omitempty"`
	// Enabled gates whether this source contributes to the team's runs.
	Enabled bool `bson:"enabled" json:"enabled"`

	CreatedBy string    `bson:"created_by" json:"created_by"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Store persists plugin sources. Mongo-backed in cloud, memory in tests.
type Store interface {
	Create(ctx context.Context, s PluginSource) error
	Get(ctx context.Context, id string) (PluginSource, error)
	Update(ctx context.Context, s PluginSource) error
	Delete(ctx context.Context, id string) error
	// ListEnabledByTenant returns the sources a launch must resolve.
	ListEnabledByTenant(ctx context.Context, tenantID string) ([]PluginSource, error)
	ListByTenant(ctx context.Context, tenantID string) ([]PluginSource, error)
}

var (
	ErrNotFound      = errors.New("pluginsource: not found")
	ErrTenantMissing = errors.New("pluginsource: store called without tenant context")
	ErrNameConflict  = errors.New("pluginsource: a source with this name already exists for the team")
)

// Validate checks a source before it is persisted. Errors are explicit —
// a malformed source must fail at write time, not silently contribute nothing
// at launch time.
func (s *PluginSource) Validate() error {
	if s.TenantID == "" {
		return ErrTenantMissing
	}
	if err := ValidName(s.Name); err != nil {
		return err
	}
	if s.GitURL == "" {
		return fmt.Errorf("pluginsource: git_url is required")
	}
	if !isSupportedGitURL(s.GitURL) {
		return fmt.Errorf("pluginsource: git_url %q must be http(s):// or ssh (git@host:path)", s.GitURL)
	}
	if s.Ref == "" {
		return fmt.Errorf("pluginsource: ref is required (pin a tag or sha)")
	}
	return nil
}

// PinnedRef reports whether Ref looks like an immutable pin (a full sha or a
// tag) rather than a moving branch. Callers use it to warn: a moving ref is
// allowed but means the plugin can change under a run without any operator
// action.
func (s *PluginSource) PinnedRef() bool {
	if len(s.Ref) == 40 && isHex(s.Ref) {
		return true
	}
	// Conventional tag shapes: v1.2.3, 1.2.3, release-2026-07-20.
	return strings.HasPrefix(s.Ref, "v") || strings.Contains(s.Ref, ".")
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func isSupportedGitURL(u string) bool {
	return strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "ssh://") ||
		(strings.Contains(u, "@") && strings.Contains(u, ":"))
}

// ValidName mirrors the plugin registry's naming rule so a source cannot
// persist a name the registry would later reject: lowercase alphanumerics plus
// '-' and '_', no path separators (the name becomes a directory).
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("pluginsource: name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("pluginsource: name %q exceeds 64 characters", name)
	}
	for _, c := range name {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("pluginsource: name %q must be lowercase alphanumeric, '-' or '_'", name)
		}
	}
	return nil
}
