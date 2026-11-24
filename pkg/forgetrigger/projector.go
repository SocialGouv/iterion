package forgetrigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// Projector writes the forge subscriptions an integration derives to the
// subscription store. It is the forge.SubscriptionProjector the server wires
// on the orchestrator, which calls it inside the integration's provision
// fence: every store call it makes takes the context it was handed, so its
// rows commit or roll back with that fence.
type Projector struct {
	Store trigger.SubscriptionStore
	// Roles resolves the deployment's role bots at projection time.
	Roles func(context.Context) RoleBots
	// Clock stamps the rows a projection writes; nil reads the wall clock.
	Clock func() time.Time
}

var _ forge.SubscriptionProjector = Projector{}

// Check plans the integration the way SyncIntegration does and returns the
// same report and refusal, writing nothing.
func (p Projector) Check(ctx context.Context, in forge.SubscriptionInput) (forge.SubscriptionReport, error) {
	plan, err := p.plan(ctx, in)
	return plan.Report, err
}

// plan is the judgement SyncIntegration and Check share: the projector's
// wiring, the integration's id, then PlanIntegration's refusal.
func (p Projector) plan(ctx context.Context, in forge.SubscriptionInput) (Plan, error) {
	if p.Store == nil {
		return Plan{}, errors.New("forgetrigger: the projector has no subscription store")
	}
	if p.Roles == nil {
		return Plan{}, errors.New("forgetrigger: the projector has no role-bot resolver: the legacy winner and the digest of its rows depend on the roles")
	}
	if in.Integration.ID == "" {
		return Plan{}, errors.New("forgetrigger: the integration to project carries no id")
	}
	plan := PlanIntegration(in, p.Roles(ctx), p.now())
	return plan, plan.Err
}

// SyncIntegration makes the stored rows of the integration's origin match
// its plan (PlanIntegration): it creates the rows the store lacks, rewrites
// the projected fields of the ones that differ, and deletes the ones the
// plan no longer holds. It reads and deletes by the integration's origin
// only, so a row an operator authored against the same binding, another
// integration's rows and another tenant's are never touched.
//
// A refused plan returns its *ProjectionError beside the report that names
// what refused it, and writes nothing. Syncing twice writes once: a row
// whose projected fields already match is left alone, and a row created or
// rewritten by a concurrent sync of the same plan is found equal.
func (p Projector) SyncIntegration(ctx context.Context, in forge.SubscriptionInput) (forge.SubscriptionReport, error) {
	plan, err := p.plan(ctx, in)
	if err != nil {
		return plan.Report, err
	}
	ri := in.Integration
	origin := trigger.ForgeOrigin(ri.ID)
	stored, err := p.Store.ListByOrigin(ctx, ri.TenantID, origin)
	if err != nil {
		return plan.Report, fmt.Errorf("forgetrigger: list the subscriptions of integration %s: %w", ri.ID, err)
	}
	storedByID := make(map[string]trigger.Subscription, len(stored))
	for _, row := range stored {
		storedByID[row.ID] = row
	}
	// A label a bot declares goes to that bot, so a manifest that gains a
	// label takes it from the residual winner with no operator gesture: the
	// report names every such transfer.
	plan.Report.Warnings = append(plan.Report.Warnings, ResidualTransfers(stored, plan)...)
	wanted := make(map[string]bool, len(plan.Rows))
	for _, want := range plan.Rows {
		wanted[want.ID] = true
		cur, found := storedByID[want.ID]
		if err := p.write(ctx, origin, cur, found, want); err != nil {
			return plan.Report, fmt.Errorf("forgetrigger: project bot %s on integration %s: %w", want.BotID, ri.ID, err)
		}
	}
	for _, row := range stored {
		if wanted[row.ID] {
			continue
		}
		// A row already gone is the outcome this delete wants: a concurrent
		// sync of the same plan removed it.
		if err := p.Store.Delete(ctx, row.ID); err != nil && !errors.Is(err, trigger.ErrSubscriptionNotFound) {
			return plan.Report, fmt.Errorf("forgetrigger: remove subscription %s (bot %s) of integration %s: %w", row.ID, row.BotID, ri.ID, err)
		}
	}
	return plan.Report, nil
}

// DropIntegration deletes every row of the integration's origin.
func (p Projector) DropIntegration(ctx context.Context, tenantID, integrationID string) error {
	if p.Store == nil {
		return errors.New("forgetrigger: the projector has no subscription store")
	}
	if integrationID == "" {
		return errors.New("forgetrigger: the integration to drop carries no id")
	}
	stored, err := p.Store.ListByOrigin(ctx, tenantID, trigger.ForgeOrigin(integrationID))
	if err != nil {
		return fmt.Errorf("forgetrigger: list the subscriptions of integration %s: %w", integrationID, err)
	}
	for _, row := range stored {
		if err := p.Store.Delete(ctx, row.ID); err != nil && !errors.Is(err, trigger.ErrSubscriptionNotFound) {
			return fmt.Errorf("forgetrigger: remove subscription %s (bot %s) of integration %s: %w", row.ID, row.BotID, integrationID, err)
		}
	}
	return nil
}

func (p Projector) now() time.Time {
	if p.Clock != nil {
		return p.Clock()
	}
	return time.Now().UTC()
}

// write lands one planned row. cur is the stored row of its id, when found.
//
// The row is created with Create, not EnsureDerived: a forge row is disabled
// through the integration's bot_triggers, never deleted, so a tombstone left
// on its id does not hold it back. A creation that fails because a row of
// the id stands now — a concurrent sync created it — reconciles that row.
//
// An existing row is rewritten only when a projected field differs, against
// the version it was read at. A row that moved under the rewrite — a version
// conflict, or a row deleted meanwhile — is read again and tried once more,
// which finds a concurrent sync's identical write equal and creates again a
// row that is gone.
func (p Projector) write(ctx context.Context, origin string, cur trigger.Subscription, found bool, want trigger.Subscription) error {
	// Two passes: the second is the one retry a row that moved is owed.
	var moved error
	for range 2 {
		if !found {
			createErr := p.Store.Create(ctx, want)
			if createErr == nil {
				return nil
			}
			fresh, err := p.Store.Get(ctx, want.ID)
			if err != nil {
				return fmt.Errorf("create subscription %s: %w", want.ID, createErr)
			}
			cur, found = fresh, true
		}
		if cur.Origin != origin {
			return fmt.Errorf("subscription %s is stored under origin %q, not %q: it is not this integration's to rewrite", cur.ID, cur.Origin, origin)
		}
		if sameProjectedFields(cur, want) {
			return nil
		}
		err := p.Store.Update(ctx, withProjectedFields(cur, want))
		switch {
		case err == nil:
			return nil
		case errors.Is(err, trigger.ErrSubscriptionNotFound):
			found = false
		case errors.Is(err, trigger.ErrSubscriptionConflict):
			fresh, getErr := p.Store.Get(ctx, want.ID)
			switch {
			case errors.Is(getErr, trigger.ErrSubscriptionNotFound):
				found = false
			case getErr != nil:
				return fmt.Errorf("re-read subscription %s after a version conflict: %w", want.ID, getErr)
			default:
				cur = fresh
			}
		default:
			return fmt.Errorf("rewrite subscription %s: %w", want.ID, err)
		}
		moved = err
	}
	return fmt.Errorf("rewrite subscription %s: it moved again under the one retry: %w", want.ID, moved)
}

// withProjectedFields lays the fields a projection owns over the stored row
// and keeps everything else the row holds: its creation stamps, its version,
// its health marks and the settings an operator pinned on it. The tenant and
// the origin are not among them: they are what the row was selected by.
func withProjectedFields(cur, want trigger.Subscription) trigger.Subscription {
	next := cur
	next.Repo = want.Repo
	next.BotID = want.BotID
	next.Invocation = want.Invocation
	next.Mode = want.Mode
	next.Match = want.Match
	next.ArgsVar = want.ArgsVar
	next.Vars = want.Vars
	next.OpensMR = want.OpensMR
	next.ForgeBinding = want.ForgeBinding
	next.Enabled = want.Enabled
	next.DisabledReason = want.DisabledReason
	next.ProjectionDigest = want.ProjectionDigest
	next.UpdatedAt = want.UpdatedAt
	return next
}

// projectedFields is the part of a row a projection owns, in a form where
// an absent and an empty value read alike: a document round-trip turns one
// into the other, and must not read as a change to project.
type projectedFields struct {
	Repo             string                `json:"repo,omitempty"`
	BotID            string                `json:"bot_id,omitempty"`
	Invocation       bundle.InvocationKind `json:"invocation,omitempty"`
	Mode             bundle.ExecutionMode  `json:"mode,omitempty"`
	Match            trigger.Matcher       `json:"match"`
	ArgsVar          string                `json:"args_var,omitempty"`
	Vars             map[string]string     `json:"vars,omitempty"`
	OpensMR          bool                  `json:"opens_mr,omitempty"`
	ForgeBinding     string                `json:"forge_binding,omitempty"`
	Enabled          bool                  `json:"enabled,omitempty"`
	DisabledReason   string                `json:"disabled_reason,omitempty"`
	ProjectionDigest string                `json:"projection_digest,omitempty"`
}

func sameProjectedFields(a, b trigger.Subscription) bool {
	view := func(s trigger.Subscription) []byte {
		// The view holds only strings, booleans, slices and string maps:
		// its encoding cannot fail.
		raw, _ := json.Marshal(projectedFields{
			Repo: s.Repo, BotID: s.BotID, Invocation: s.Invocation, Mode: s.Mode,
			Match: s.Match, ArgsVar: s.ArgsVar, Vars: s.Vars, OpensMR: s.OpensMR,
			ForgeBinding: s.ForgeBinding, Enabled: s.Enabled,
			DisabledReason: s.DisabledReason, ProjectionDigest: s.ProjectionDigest,
		})
		return raw
	}
	return bytes.Equal(view(a), view(b))
}
