package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// decodePoolBlock is the ONE decoder of a provisioning record's pool:
// request field (ADR-164) — the schedule create/PATCH and the forge
// integration enable/PATCH, the twin of decodeRoutingBlock. ABSENT (no
// bytes) = the request said nothing (spoke=false, the stored block
// stands); `null` = a spoken clear (nil, true); an object = a spoken
// replace. The decode is STRICT (DisallowUnknownFields): a camelCase typo
// refuses rather than storing a block whose dial silently reads as unset.
// Shape only — the pool's existence and consent are validatePoolBindingWrite's.
func decodePoolBlock(raw json.RawMessage) (*credpool.Binding, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, true, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b credpool.Binding
	if err := dec.Decode(&b); err != nil {
		return nil, false, fmt.Errorf("pool: %w", err)
	}
	return &b, true, nil
}

// validatePoolBindingWrite checks a record's pool block for the write —
// every surface that stores one (schedule, forge integration) calls it,
// and the org-approval apply re-runs it, because consent read at park
// time may be revoked by the time an admin approves: shape, triggers,
// the pool's existence and enablement, and the pool's CONSENT (its owner
// scope, or an audience admitting the team's org). The launch-time fold
// still refuses a pool that vanished since (HasPool).
func (s *Server) validatePoolBindingWrite(ctx context.Context, teamID string, b *credpool.Binding) error {
	if b == nil {
		return nil
	}
	if err := b.Validate(); err != nil {
		return err
	}
	// Triggers validated at the write (the fold backstops, never replaces)
	// — the same guard the attachment routes carry.
	if err := validatePoolTriggers(b.Strategy.Triggers); err != nil {
		return err
	}
	// An OPT-OUT block (enabled=false) grants nothing — it stops the fold
	// at this level, whatever the named pool's state or owner — so it needs
	// no consent: refusing it would make "keep this repo out of the
	// platform's pool" unwritable.
	if !b.Enabled {
		return nil
	}
	if s.credPoolPools == nil {
		return fmt.Errorf("pool attachments are not wired on this deployment")
	}
	pool, err := s.credPoolPools.Get(ctx, b.PoolID)
	if err != nil {
		return fmt.Errorf("the pool block names pool %q, which does not exist", b.PoolID)
	}
	if !pool.Enabled {
		return fmt.Errorf("the pool block names pool %q, which is disabled", b.PoolID)
	}
	if pool.EffectiveVisibility() == credpool.PoolVisibilityPrivate {
		// A private pool consents ONLY by ownership, and a record's block
		// is a team's MACHINE lane (a schedule, a forge integration's
		// webhook): user-private pools do not serve machine lanes.
		return fmt.Errorf("pool %q is private: it serves no machine lane — attach it to a team of its owner instead", b.PoolID)
	}
	if pool.EffectiveOwnerType() == credpool.PoolOwnerPlatform {
		// The instance pool serves the deployment's own admins only
		// (poolAttachmentConsent's rule, mirrored): a team record may
		// never bind it — its entries are the paid gateway defaults.
		return fmt.Errorf("pool %q is the platform's instance pool — only the platform scope binds it", b.PoolID)
	}
	if pool.EffectiveOwnerType() == credpool.PoolOwnerOrg {
		t, gerr := s.authStore().GetTeam(ctx, teamID)
		if gerr != nil || t.OrgID == "" {
			return fmt.Errorf("team %s resolves no org: the org pool %q cannot consent", teamID, b.PoolID)
		}
		if pool.OrgID != t.OrgID {
			return fmt.Errorf("pool %q belongs to org %q, not this team's org", b.PoolID, pool.OrgID)
		}
		return nil
	}
	// A USER pool: its audience must admit the team (the same dial
	// poolAttachmentConsent reads) — private pools were already refused
	// above. The audience may name the team itself, so a team outside any
	// org can be admitted; only an unreadable team refuses.
	t, gerr := s.authStore().GetTeam(ctx, teamID)
	if gerr != nil {
		return fmt.Errorf("team %s could not be read: the user pool %q cannot consent: %w", teamID, b.PoolID, gerr)
	}
	if !pool.Audience.Allows(pool.OrgID, t.OrgID, teamID, "", false) {
		return fmt.Errorf("pool %q's audience does not admit team %s — the pool owner widens it first", b.PoolID, teamID)
	}
	return nil
}

// decodeAndValidatePoolBlock decodes a request's pool: field and, when it
// carries a block, validates it for the write — the one pair every record
// surface runs. Returns the block, whether the request spoke at all (a
// `null` is a spoken clear) and the refusal.
func (s *Server) decodeAndValidatePoolBlock(ctx context.Context, teamID string, raw json.RawMessage) (*credpool.Binding, bool, error) {
	b, spoke, err := decodePoolBlock(raw)
	if err != nil {
		return nil, false, err
	}
	if err := s.validatePoolBindingWrite(ctx, teamID, b); err != nil {
		return nil, false, err
	}
	return b, spoke, nil
}

// poolLayersOf projects an integration's webhook config pool block into the
// fold's binding layers — the ONE projection every lane that launches or
// previews on behalf of an integration passes: the direct webhook lane,
// the board and pipeline card lanes, the webhook-source credential
// preview. An opt-out block (enabled=false) is a layer too: it stops the
// fold there. Nil config or no block = no layer.
func poolLayersOf(cfg *webhooks.Config) []credpool.BindingLayer {
	if cfg == nil {
		return nil
	}
	if bl, ok := cfg.PoolBindingLayer(); ok {
		return []credpool.BindingLayer{bl}
	}
	return nil
}

// cardPoolLayers is poolLayersOf for a CARD lane: the card names the
// integration through a member-writable bot-arg, and a member may re-bind
// the card to another bot while the id stays. The integration's block
// speaks only for the bots it runs and only while it is enabled — for any
// other bot the card's run folds the stored scopes like any team run.
//
// An OPT-OUT block always speaks: it grants nothing, and dropping it would
// let a carded run of this repo draw the team's pool while the same
// integration's routing and var layers still apply.
func cardPoolLayers(cfg *webhooks.Config, botID string) []credpool.BindingLayer {
	if cfg != nil && cfg.Pool != nil && !cfg.Pool.Enabled {
		return poolLayersOf(cfg)
	}
	if cfg == nil || !cfg.Enabled || !cfg.AllowsBot(botID) {
		return nil
	}
	return poolLayersOf(cfg)
}
