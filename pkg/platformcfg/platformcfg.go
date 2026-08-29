// Package platformcfg holds platform-scoped runtime-settings families
// beyond the usage caps that established the doctrine (ADR-090): env var =
// deployment default, DB record = runtime override effective without a
// restart, super-admin API/CLI as the write surface. Each family is one
// document in the shared `platform_settings` Mongo collection, keyed by a
// fixed _id (the layout usagecap's settings store reserved for exactly
// this growth).
//
// Two families ship here:
//   - bot_roles — the webhook role→bot-id bindings that were hardcoded
//     engine constants (reviewer/revi_converse/brancher/implementer), so
//     re-pointing a role at another bot no longer needs a rollout.
//   - sandbox — the `sandbox: auto` fallback image, resolved at PUBLISH
//     time and pinned on the RunMessage so a redelivery reruns in the same
//     environment.
//
// A nil field means "no override — inherit the code/env default", which is
// what keeps a deployment that never touches the API at exactly its
// baked-in behaviour.
package platformcfg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
)

// BotRoles is the role→bot-id override record. Each field, when non-nil,
// replaces the corresponding hardcoded default at every consuming site
// (webhook auto-review fan-out, /revi approve, the merge-queue auto-heal,
// the issue-labeled implementer lane).
type BotRoles struct {
	// Reviewer handles PR/MR review (default "review-pr").
	Reviewer *string `bson:"reviewer,omitempty" json:"reviewer"`
	// ReviConverse answers conversational /revi questions (default
	// "revi-converse").
	ReviConverse *string `bson:"revi_converse,omitempty" json:"revi_converse"`
	// Brancher improves an existing branch (default "branch-improve-loop").
	Brancher *string `bson:"brancher,omitempty" json:"brancher"`
	// Implementer handles issue-labeled feature work (default "feature-dev").
	Implementer *string `bson:"implementer,omitempty" json:"implementer"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// Validate rejects override values that could never resolve to a bot: role
// ids follow the bot-slug rule (lowercase alphanumerics, '-', '_').
func (r BotRoles) Validate() error {
	for name, v := range map[string]*string{
		"reviewer": r.Reviewer, "revi_converse": r.ReviConverse,
		"brancher": r.Brancher, "implementer": r.Implementer,
	} {
		if v == nil {
			continue
		}
		if *v == "" {
			return fmt.Errorf("platformcfg: %s: bot id must not be empty (clear the override instead)", name)
		}
		// The one slug grammar bots are actually stored under — a role value
		// that could not name a stored/baked bot must fail at write time.
		if err := botsource.ValidSlug(*v); err != nil {
			return fmt.Errorf("platformcfg: %s: %w", name, err)
		}
	}
	return nil
}

// Sandbox is the sandbox runtime-settings record.
type Sandbox struct {
	// DefaultImage overrides ITERION_SANDBOX_DEFAULT_IMAGE / the built-in
	// version-pinned image as the `sandbox: auto` fallback. Cloud guidance:
	// use an @sha256 digest ref so the pinned message stays reproducible
	// against a mutable tag push too.
	DefaultImage *string `bson:"default_image,omitempty" json:"default_image"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// EffectiveImage resolves the override to its effective value: "" (inherit
// the env default / built-in pin) when unset, the trimmed ref otherwise.
// The ONE definition every consumer (server echo, publisher pin) shares.
func (s *Sandbox) EffectiveImage() string {
	if s == nil || s.DefaultImage == nil {
		return ""
	}
	return strings.TrimSpace(*s.DefaultImage)
}

// Validate rejects a blank override (clearing is expressed by nil, never by
// an empty string that would pin "no image" onto every RunMessage).
func (s Sandbox) Validate() error {
	if s.DefaultImage != nil && strings.TrimSpace(*s.DefaultImage) == "" {
		return fmt.Errorf("platformcfg: default_image override must not be blank (clear it to fall back to the env default)")
	}
	return nil
}

// BotVars is the bot-variable override record: the `${ITERION_X:-default}`
// expansions every .bot declares (model pins, reasoning effort, tunables)
// resolved from the DB before the pod's env — so re-tuning a bot no longer
// needs a Helm values change and a rollout. The record stays typed and
// validated here per the ADR-090 rejection of a generic KV table; only the
// VALUE SPACE is a map, because the keys are declared by bots, not by the
// engine.
type BotVars struct {
	// Vars maps ITERION_* names to their override values. A key present
	// here beats the pod's env var; an absent key inherits env then the
	// .bot's `:-` default. Clearing = removing the key (never an empty
	// value).
	Vars map[string]string `bson:"vars,omitempty" json:"vars"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// botVarsInfraPrefixes are the env namespaces a DB record must never
// shadow: they configure the process's own transport, storage, identity
// and enforcement — an override there would let a settings write repoint
// the database it is stored in, or blunt the cap that meters it. The
// usage-cap and sandbox-image families are excluded because they already
// have their own audited, validated settings surface.
var botVarsInfraPrefixes = []string{
	"ITERION_MONGO_", "ITERION_NATS_", "ITERION_JWT_", "ITERION_SECRETS_",
	"ITERION_S3_", "ITERION_BLOB_", "ITERION_VALKEY_", "ITERION_OIDC_",
	"ITERION_OAUTH_", "ITERION_USAGE_CAP", "ITERION_SANDBOX_DEFAULT_IMAGE",
	"ITERION_PUBLIC_URL", "ITERION_BOOTSTRAP_",
}

// botVarsMax bounds the record so a runaway writer cannot grow the
// document the TTL cache re-reads on every expiry.
const (
	botVarsMaxKeys     = 200
	botVarsMaxValueLen = 256
)

// Validate rejects names outside the bot-var grammar, credential-shaped
// names (the same doctrine as the model-env allowlist: a var whose name
// suggests a secret must never transit a non-secret surface), infra
// namespaces, and unbounded values. One bad entry fails the whole write —
// nothing is persisted partially.
func (b BotVars) Validate() error {
	if len(b.Vars) > botVarsMaxKeys {
		return fmt.Errorf("platformcfg: bot_vars: %d keys exceeds the %d-key bound", len(b.Vars), botVarsMaxKeys)
	}
	for name, val := range b.Vars {
		if !botVarNameOK(name) {
			return fmt.Errorf("platformcfg: bot_vars: %q is not an overridable bot var (want ITERION_A_Z0_9 outside the infra/credential namespaces)", name)
		}
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("platformcfg: bot_vars: %s: value must not be blank (remove the key to clear the override)", name)
		}
		if strings.ContainsAny(val, "\n\r") {
			return fmt.Errorf("platformcfg: bot_vars: %s: value must be a single line", name)
		}
		if len(val) > botVarsMaxValueLen {
			return fmt.Errorf("platformcfg: bot_vars: %s: value exceeds %d bytes", name, botVarsMaxValueLen)
		}
	}
	return nil
}

// botVarNameOK is the name gate: ITERION_-prefixed upper snake case, no
// credential-shaped words, no infra namespace.
func botVarNameOK(name string) bool {
	if !strings.HasPrefix(name, "ITERION_") || len(name) <= len("ITERION_") {
		return false
	}
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	for _, bad := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PRIVATE", "CREDENTIAL"} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	for _, p := range botVarsInfraPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// Store persists one settings family: Get returns (nil, nil) when no record
// exists yet; Put replaces the whole record (ReplaceOne semantics, so a
// cleared override really disappears).
type Store[T any] interface {
	Get(ctx context.Context) (*T, error)
	Put(ctx context.Context, rec T) error
}

// DefaultTTL bounds how long a replica serves a cached family record. The
// mutating replica invalidates immediately; the others converge within the
// TTL (the ADR-090 read-cache posture — Mongo stays the authority).
const DefaultTTL = 30 * time.Second
