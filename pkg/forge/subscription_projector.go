package forge

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// SubscriptionInput is everything one projection of an integration's forge
// subscriptions reads: the integration as it is stored (its BotTriggers and
// its adoption stamp), the webhook config that enforces it, and the manifests
// of the bots it lists — resolved by the lookups Provision itself uses.
type SubscriptionInput struct {
	Integration RepoIntegration
	Config      webhooks.Config
	// Bots is the integration's bot set, sorted.
	Bots     []string
	InvByBot map[string][]bundle.Invocation
	FrByBot  map[string]*bundle.ForgeRequirements
}

// SubscriptionReport is what one projection decided.
type SubscriptionReport struct {
	// Enabled maps lane → bot → the labels its enabled rows claim ("*" = any).
	Enabled map[string]map[string][]string `json:"enabled,omitempty"`
	// Disabled maps a bot to the reason its rows are derived disabled.
	Disabled map[string]string `json:"disabled,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	// AdoptionPins are the per-bot states a first projection freezes. The
	// caller persists them, inside the fence the projection ran in
	// (RepoIntegrationStore.StampSubscriptionSync).
	AdoptionPins map[string]BotTrigger `json:"adoption_pins,omitempty"`
	// Digest is the projection digest stamped on every row.
	Digest string `json:"digest,omitempty"`
	// HandPinned are the config launch-var keys that differ from the union of
	// the bots' manifest vars.
	HandPinned []string `json:"hand_pinned,omitempty"`
	// Overlaps names, as "bot: row a ∩ row b", the issue rows of one bot whose
	// label sets may intersect. A projection that reports any is refused.
	Overlaps []string `json:"overlaps,omitempty"`
}

// SubscriptionProjector writes the forge-bound trigger subscriptions an
// integration derives. It is the seam between this package and the trigger
// spine, which imports this package and so cannot be imported from it.
//
// Both methods run inside the integration's provision fence, with the fence's
// context: an implementation passes that context to every store write it
// makes, so its rows commit or roll back with the fence. The Mongo fence may
// run its callback more than once, so SyncIntegration is idempotent.
type SubscriptionProjector interface {
	// SyncIntegration rewrites the integration's derived rows to match in and
	// reports what it decided. A refused projection returns its error and
	// whatever the report already names (Overlaps), and leaves the stored
	// rows as they were.
	SyncIntegration(ctx context.Context, in SubscriptionInput) (SubscriptionReport, error)
	// DropIntegration deletes every row the integration derived.
	DropIntegration(ctx context.Context, tenantID, integrationID string) error
}

// ErrSubscriptionProjection marks a projection the SubscriptionProjector
// refused or could not write. It wraps the projector's own error, so a caller
// still reads that error's type through errors.As.
var ErrSubscriptionProjection = errors.New("forge: subscription projection failed")

// AnyLaneRoutesSpine reports whether a stored routing puts any lane on the
// spine. It reads what is stored: a lane left unset is decided by the
// server's default, which this package does not hold.
func AnyLaneRoutesSpine(r webhooks.ForgeRouting) bool {
	return r.Issues == webhooks.RoutingSpine || r.PullRequests == webhooks.RoutingSpine || r.Comments == webhooks.RoutingSpine
}

// setRoutingLane writes mode on the lane named lane and reports whether lane
// names one.
func setRoutingLane(r *webhooks.ForgeRouting, lane, mode string) bool {
	switch lane {
	case webhooks.LaneIssues:
		r.Issues = mode
	case webhooks.LanePullRequests:
		r.PullRequests = mode
	case webhooks.LaneComments:
		r.Comments = mode
	default:
		return false
	}
	return true
}

// applyBotTriggers returns the stored per-bot triggers with the request's
// entries laid over them: an entry replaces that bot's whole value, and a bot
// the request does not name keeps what it has. A nil request therefore changes
// nothing. The result shares no map or slice with either argument, and is nil
// when it holds no entry.
func applyBotTriggers(stored, request map[string]BotTrigger) map[string]BotTrigger {
	if len(stored)+len(request) == 0 {
		return nil
	}
	out := make(map[string]BotTrigger, len(stored)+len(request))
	for bot, t := range stored {
		out[bot] = cloneBotTrigger(t)
	}
	for bot, t := range request {
		out[bot] = cloneBotTrigger(t)
	}
	return out
}

func cloneBotTrigger(t BotTrigger) BotTrigger {
	// slices.Clone keeps a nil slice nil and an empty one empty: the two
	// states of Labels stay apart.
	t.Labels = slices.Clone(t.Labels)
	if t.Enabled != nil {
		enabled := *t.Enabled
		t.Enabled = &enabled
	}
	t.LaunchVars = maps.Clone(t.LaunchVars)
	return t
}

// botTriggersEqual compares two per-bot trigger maps as values. A nil map and
// an empty one are equal; a nil Labels and an empty one are not — the first
// follows the manifest, the second claims any label.
func botTriggersEqual(a, b map[string]BotTrigger) bool {
	if len(a) != len(b) {
		return false
	}
	for bot, x := range a {
		y, ok := b[bot]
		if !ok || !botTriggerEqual(x, y) {
			return false
		}
	}
	return true
}

func botTriggerEqual(x, y BotTrigger) bool {
	if (x.Labels == nil) != (y.Labels == nil) || !slices.Equal(x.Labels, y.Labels) {
		return false
	}
	if (x.Enabled == nil) != (y.Enabled == nil) || (x.Enabled != nil && *x.Enabled != *y.Enabled) {
		return false
	}
	return x.Residual == y.Residual && maps.Equal(x.LaunchVars, y.LaunchVars)
}

// BotTriggersExpandSurface is the org-approval judgment of a bot_triggers
// write: true when laying requested over stored (the rule Provision applies)
// may start a bot on an event that does not start it today, or changes what
// its runs are told. It fails toward approval: where the stored state does not
// say whether a change widens, it does.
//
// Per requested bot, against the entry it replaces (none = the zero entry):
//   - labels: a pattern the stored set does not hold, the any-label set, or a
//     return to the manifest's labels from an explicit set all expand; so does
//     a first explicit set, which enables a bot the enable rule had left off.
//     A subset of the stored patterns, or anything under a stored any-label
//     set, narrows;
//   - enabled: true expands unless it already was; dropping an explicit false
//     hands the bot back to the enable rule, which may turn it on;
//   - residual: turning it on expands;
//   - launch_vars: any difference expands — a var decides what a run does,
//     and nothing here ranks one value against another.
func BotTriggersExpandSurface(stored, requested map[string]BotTrigger) bool {
	for bot, next := range requested {
		prev := stored[bot]
		if botTriggerLabelsWiden(prev.Labels, next.Labels) {
			return true
		}
		wasOn := prev.Enabled != nil && *prev.Enabled
		wasOff := prev.Enabled != nil && !*prev.Enabled
		switch {
		case next.Enabled != nil && *next.Enabled && !wasOn:
			return true
		case next.Enabled == nil && wasOff:
			return true
		}
		if next.Residual && !prev.Residual {
			return true
		}
		if !maps.Equal(prev.LaunchVars, next.LaunchVars) {
			return true
		}
	}
	return false
}

// botTriggerLabelsWiden reports whether replacing prev by next may claim a
// label prev did not. Patterns are compared as webhooks.MatchLabel reads
// them: trimmed and case-insensitive, "*" claiming everything.
func botTriggerLabelsWiden(prev, next []string) bool {
	switch {
	case next == nil:
		// Back to the manifest's labels: unknown from here, so it widens
		// whenever the bot held an explicit set.
		return prev != nil
	case prev == nil:
		// A first explicit set is the gesture that enables a bot.
		return true
	}
	held := make(map[string]bool, len(prev))
	for _, p := range prev {
		held[strings.ToLower(strings.TrimSpace(p))] = true
	}
	if len(prev) == 0 || held["*"] {
		// The stored set already claims every label.
		return false
	}
	if len(next) == 0 {
		return true
	}
	for _, p := range next {
		if !held[strings.ToLower(strings.TrimSpace(p))] {
			return true
		}
	}
	return false
}

// resolveBotManifests reads, for each bot, its forge requirements and its
// invocations through the orchestrator's two lookups. It is the one resolution
// behind every path that derives something from the bots of an integration —
// Provision, the on-demand subscription sync and the routing switch — so they
// cannot read a bot differently. A bot that declares neither a forge: block
// nor a forge/command invocation is refused with ErrBotNotProvisionable.
func (o *Orchestrator) resolveBotManifests(ctx context.Context, tenantID string, bots []string) (map[string]*bundle.ForgeRequirements, map[string][]bundle.Invocation, error) {
	frByBot := make(map[string]*bundle.ForgeRequirements, len(bots))
	invByBot := make(map[string][]bundle.Invocation, len(bots))
	for _, b := range bots {
		fr, err := o.Bots(ctx, tenantID, b)
		if err != nil {
			return nil, nil, fmt.Errorf("forge: resolve bot %q: %w", b, err)
		}
		var invs []bundle.Invocation
		if o.Invocations != nil {
			if invs, err = o.Invocations(ctx, tenantID, b); err != nil {
				return nil, nil, fmt.Errorf("forge: resolve invocations for %q: %w", b, err)
			}
		}
		if fr == nil && !hasForgeReachableInvocation(invs) {
			return nil, nil, fmt.Errorf("%w: bot %q declares neither a forge: block nor a forge/command invocation; cannot auto-provision", ErrBotNotProvisionable, b)
		}
		frByBot[b] = fr
		invByBot[b] = invs
	}
	return frByBot, invByBot, nil
}

// projectSubscriptions runs one projection and stamps its outcome on the
// integration. It runs INSIDE a provision fence: ctx is the fence's context,
// and the stamp is the targeted write that context allows.
//
// A refused projection follows failOnRefusal. True — the integration routes,
// or is about to route, a lane by these rows — returns the refusal, and the
// fence rolls back. False records the refusal on the integration, warns, and
// returns it as refused beside a nil error, so whatever shares the fence still
// commits: the rows are inert while nothing routes by them.
//
// A zero in.Config (the integration names a webhook config that does not
// exist) is a refusal like any other: there is nothing to project from.
func (o *Orchestrator) projectSubscriptions(ctx context.Context, in SubscriptionInput, failOnRefusal bool) (report SubscriptionReport, refused, err error) {
	if in.Config.ID == "" {
		refused = fmt.Errorf("integration %s names webhook config %q, which does not exist", in.Integration.ID, in.Integration.WebhookID)
	} else {
		report, refused = o.Subscriptions.SyncIntegration(ctx, in)
	}
	if refused != nil {
		refused = fmt.Errorf("%w: integration %s: %w", ErrSubscriptionProjection, in.Integration.ID, refused)
		if failOnRefusal {
			return report, nil, refused
		}
		if serr := o.Integrations.StampSubscriptionSync(ctx, in.Integration.ID, nil, o.clock(), refused.Error()); serr != nil {
			return report, nil, errors.Join(refused, fmt.Errorf("forge: record the refused projection on integration %s: %w", in.Integration.ID, serr))
		}
		if o.LogWarn != nil {
			o.LogWarn("forge: integration %s (%s): its forge subscriptions were not projected, and no lane routes by them: %v", in.Integration.ID, in.Integration.RepoFullName, refused)
		}
		return report, refused, nil
	}
	if err := o.Integrations.StampSubscriptionSync(ctx, in.Integration.ID, report.AdoptionPins, o.clock(), ""); err != nil {
		return report, nil, fmt.Errorf("forge: stamp the subscription sync on integration %s: %w", in.Integration.ID, err)
	}
	return report, nil, nil
}

// SyncSubscriptions re-projects one integration's forge subscriptions on
// demand, from the manifests as the catalogue serves them now. It takes the
// provision lease and runs the projection and its stamp in one fence.
//
// A refused projection is returned, wrapped in ErrSubscriptionProjection,
// beside the report that names what refused it. On an integration that routes
// no lane by the spine the refusal is also recorded on it
// (SubscriptionsSyncError); on one that does, the fence rolls back and the
// stored rows stand.
func (o *Orchestrator) SyncSubscriptions(ctx context.Context, tenantID, integrationID string) (SubscriptionReport, error) {
	if o.Subscriptions == nil {
		return SubscriptionReport{}, fmt.Errorf("%w: no subscription projector is wired on this server", ErrSubscriptionProjection)
	}
	ri, err := o.Integrations.Get(ctx, integrationID)
	if err != nil {
		return SubscriptionReport{}, err
	}
	if ri.TenantID != tenantID {
		return SubscriptionReport{}, ErrIntegrationNotFound
	}
	ctx, release, err := o.provisionLease(ctx, ri.TenantID, ri.ConnectionID, ri.RepoFullName)
	if err != nil {
		return SubscriptionReport{}, err
	}
	defer release()
	in, err := o.subscriptionInput(ctx, integrationID)
	if err != nil {
		return SubscriptionReport{}, err
	}
	var report SubscriptionReport
	var refused error
	err = o.Integrations.WithProvisionVersion(ctx, in.Integration.ID, in.Integration.Version, false, func(ctx context.Context) error {
		var perr error
		report, refused, perr = o.projectSubscriptions(ctx, in, AnyLaneRoutesSpine(in.Integration.ForgeRouting))
		return perr
	})
	if err != nil {
		return report, err
	}
	return report, refused
}

// subscriptionInput reads, under the provision lease, what a projection of
// one stored integration needs: the integration itself (re-read, so the
// version the fence compares is the one the lease protects), its webhook
// config, and its bots' manifests.
func (o *Orchestrator) subscriptionInput(ctx context.Context, integrationID string) (SubscriptionInput, error) {
	ri, err := o.Integrations.Get(ctx, integrationID)
	if err != nil {
		return SubscriptionInput{}, err
	}
	cfg, err := o.Webhooks.Get(ctx, ri.WebhookID)
	if err != nil {
		return SubscriptionInput{}, fmt.Errorf("forge: read webhook %s of integration %s: %w", ri.WebhookID, ri.ID, err)
	}
	bots := dedupSorted(ri.BotIDs)
	frByBot, invByBot, err := o.resolveBotManifests(ctx, ri.TenantID, bots)
	if err != nil {
		return SubscriptionInput{}, err
	}
	return SubscriptionInput{Integration: ri, Config: cfg, Bots: bots, InvByBot: invByBot, FrByBot: frByBot}, nil
}
