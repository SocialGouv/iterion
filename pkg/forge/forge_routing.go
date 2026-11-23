package forge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// RoutingResult reports what SetForgeRouting left in force.
type RoutingResult struct {
	IntegrationID string `json:"integration_id"`
	WebhookID     string `json:"webhook_id"`
	Lane          string `json:"lane"`
	// Mode is the lane's mode read back from the stored webhook config — the
	// document a delivery is routed by — after the write.
	Mode string `json:"mode"`
	// Routing and SpineSince are that same config's whole switch.
	Routing    webhooks.ForgeRouting `json:"forge_routing,omitzero"`
	SpineSince *time.Time            `json:"spine_since,omitempty"`
	// Report is the projection the switch ran before it wrote.
	Report SubscriptionReport `json:"report"`
}

// The codes of a RoutingRefusedError.
const (
	// RoutingRefusedNoProjector: this server projects no forge subscription,
	// so nothing could route a spine lane.
	RoutingRefusedNoProjector = "subscriptions_not_wired"
	// RoutingRefusedProjection: the projection itself was refused, for a
	// reason other than overlapping rows.
	RoutingRefusedProjection = "projection_refused"
	// RoutingRefusedOverlappingRows: one bot derives issue rows whose label
	// sets may intersect.
	RoutingRefusedOverlappingRows = "overlapping_rows"
	// RoutingRefusedNoEnabledRow: the projection enabled no row on the lane.
	RoutingRefusedNoEnabledRow = "no_enabled_row"
	// RoutingRefusedHandPinnedLaunchVars: the webhook config's launch vars
	// differ from what the bots' manifests declare, and a spine launch does
	// not apply that map.
	RoutingRefusedHandPinnedLaunchVars = "hand_pinned_launch_vars"
)

// ErrRoutingRefused is the class of every RoutingRefusedError.
var ErrRoutingRefused = errors.New("forge: the routing switch refused to route the lane by the spine")

// RoutingRefusedError is SetForgeRouting declining to put a lane on the spine.
// Nothing was written: the fence the switch ran in rolled back. Code is one of
// the RoutingRefused* constants; Reason says what to change. No refusal of
// this type yields to force.
type RoutingRefusedError struct {
	Code   string
	Reason string
	// Report is the projection the refusal was read from; zero when no
	// projection ran.
	Report SubscriptionReport
	// Cause is the projector's own error, when the refusal relays one.
	Cause error
}

func (e *RoutingRefusedError) Error() string {
	return fmt.Sprintf("%v (%s): %s", ErrRoutingRefused, e.Code, e.Reason)
}

// Is places every refusal in the ErrRoutingRefused class.
func (e *RoutingRefusedError) Is(target error) bool { return target == ErrRoutingRefused }

// Unwrap exposes the projector's error to errors.As.
func (e *RoutingRefusedError) Unwrap() error { return e.Cause }

// ErrRoutingInvalid is the class of every RoutingInvalidError.
var ErrRoutingInvalid = errors.New("forge: invalid routing switch request")

// RoutingInvalidError is a lane or a mode SetForgeRouting does not know.
type RoutingInvalidError struct {
	// Field is "lane" or "mode".
	Field string
	Value string
	// Allowed lists the values the field takes.
	Allowed []string
}

func (e *RoutingInvalidError) Error() string {
	return fmt.Sprintf("%v: %s %q is not one of [%s]", ErrRoutingInvalid, e.Field, e.Value, strings.Join(e.Allowed, " "))
}

// Is places the error in the ErrRoutingInvalid class.
func (e *RoutingInvalidError) Is(target error) bool { return target == ErrRoutingInvalid }

var (
	routingLanes = []string{webhooks.LaneIssues, webhooks.LanePullRequests, webhooks.LaneComments}
	// The empty mode is not writable: a lane is never returned to "unset", it
	// goes back to legacy by naming legacy.
	routingModes = []string{webhooks.RoutingLegacy, webhooks.RoutingShadow, webhooks.RoutingSpine}
)

// RoutingSwitchExpandsSurface is the org-approval judgment of a routing
// switch: true when it puts on the spine a lane that is not there. On the
// spine every bot whose subscription claims an event starts, where legacy
// starts one, so the switch grows what automation does on the repo. Going
// back, moving between legacy and shadow, and re-asserting a lane already on
// the spine do not.
func RoutingSwitchExpandsSurface(stored webhooks.ForgeRouting, lane, mode string) bool {
	return mode == webhooks.RoutingSpine && stored.For(lane) != webhooks.RoutingSpine
}

// SetForgeRouting switches one lane of one integration between routers
// (ADR-172 §8). Under the provision lease it reads the integration, then opens
// one fence in which it projects the subscriptions, judges the switch, and
// writes it: on the integration, which owns it, by a targeted write, then on
// the webhook config a delivery is routed by. The result is read back from
// that config.
//
// A switch to spine is refused, with a *RoutingRefusedError and nothing
// written, when no projector is wired, when the projection is refused or
// reports overlapping rows, when it enables no row on the lane, or when the
// config's launch vars differ from the union of the bots' manifest vars — a
// spine launch layers each bot's own vars and does not read that map, so a
// hand pin in it would stop applying without a word. force lifts none of
// these.
//
// A switch away from spine is never held back by the projection: rolling back
// has to stay possible when the rows are the problem. A projection refused
// then is recorded on the integration (SubscriptionsSyncError) and warned,
// and the switch is written. SpineSince is stamped by the first switch to
// spine and kept by every later one, legacy included.
//
// An unknown lane or mode is a *RoutingInvalidError.
func (o *Orchestrator) SetForgeRouting(ctx context.Context, tenantID, integrationID, lane, mode string, force bool) (RoutingResult, error) {
	// force is the escape hatch of the shadow-parity gate, which this switch
	// does not hold; nothing it refuses today reads it.
	_ = force
	var probe webhooks.ForgeRouting
	if !setRoutingLane(&probe, lane, mode) {
		return RoutingResult{}, &RoutingInvalidError{Field: "lane", Value: lane, Allowed: routingLanes}
	}
	if !slices.Contains(routingModes, mode) {
		return RoutingResult{}, &RoutingInvalidError{Field: "mode", Value: mode, Allowed: routingModes}
	}
	toSpine := mode == webhooks.RoutingSpine

	ri, err := o.Integrations.Get(ctx, integrationID)
	if err != nil {
		return RoutingResult{}, err
	}
	if ri.TenantID != tenantID {
		return RoutingResult{}, ErrIntegrationNotFound
	}
	if toSpine && o.Subscriptions == nil {
		return RoutingResult{}, &RoutingRefusedError{Code: RoutingRefusedNoProjector,
			Reason: "this server projects no forge subscription, so no delivery could be routed by one; the lane stays as it is"}
	}
	ctx, release, err := o.provisionLease(ctx, ri.TenantID, ri.ConnectionID, ri.RepoFullName)
	if err != nil {
		return RoutingResult{}, err
	}
	defer release()
	in, err := o.subscriptionInput(ctx, integrationID)
	if err != nil {
		return RoutingResult{}, err
	}
	ri, cfg := in.Integration, in.Config

	// The config holds the integration's projection of the switch, so it is
	// written from the integration's routing with the lane set — not from its
	// own stored copy, which a failed earlier switch may have left behind.
	routing := ri.ForgeRouting
	setRoutingLane(&routing, lane, mode)
	now := o.clock()
	spineSince := ri.SpineSince
	if toSpine && spineSince == nil {
		spineSince = &now
	}

	var report SubscriptionReport
	err = o.Integrations.WithProvisionVersion(ctx, ri.ID, ri.Version, false, func(ctx context.Context) error {
		if o.Subscriptions != nil {
			var perr error
			if report, _, perr = o.projectSubscriptions(ctx, in, toSpine); perr != nil {
				// Only the projector's refusal is a refusal of the switch;
				// a stamp the store could not write is a failure, and
				// stays one.
				if toSpine && errors.Is(perr, ErrSubscriptionProjection) {
					return spineProjectionRefusal(report, perr)
				}
				return perr
			}
		}
		if toSpine {
			if refusal := judgeSpineSwitch(in, lane, report); refusal != nil {
				return refusal
			}
		}
		if err := o.Integrations.StampForgeRouting(ctx, ri.ID, lane, mode, now); err != nil {
			return fmt.Errorf("forge: write the routing switch on integration %s: %w", ri.ID, err)
		}
		next := cfg
		next.ForgeRouting = routing
		next.SpineSince = spineSince
		next.UpdatedAt = now
		if err := o.Webhooks.Update(ctx, next); err != nil {
			return fmt.Errorf("forge: write the routing switch on webhook %s: %w", cfg.ID, err)
		}
		return nil
	})
	if err != nil {
		return RoutingResult{Report: report}, err
	}
	stored, err := o.Webhooks.Get(ctx, cfg.ID)
	if err != nil {
		return RoutingResult{Report: report}, fmt.Errorf("forge: the routing switch of integration %s is written, but webhook %s could not be read back: %w", ri.ID, cfg.ID, err)
	}
	return RoutingResult{
		IntegrationID: ri.ID,
		WebhookID:     cfg.ID,
		Lane:          lane,
		Mode:          stored.ForgeRouting.For(lane),
		Routing:       stored.ForgeRouting,
		SpineSince:    stored.SpineSince,
		Report:        report,
	}, nil
}

// spineProjectionRefusal types the refusal of a projection that a switch to
// spine depends on. Overlapping rows get their own code and name the rows;
// any other refusal relays the projector's reason.
func spineProjectionRefusal(report SubscriptionReport, cause error) *RoutingRefusedError {
	if len(report.Overlaps) > 0 {
		return &RoutingRefusedError{Code: RoutingRefusedOverlappingRows, Report: report, Cause: cause,
			Reason: fmt.Sprintf("a bot must claim an event through one row, and these may claim the same label: %s — give the rows disjoint labels in the bot's manifest, or remove the bot_triggers labels that apply to all of them",
				strings.Join(report.Overlaps, "; "))}
	}
	return &RoutingRefusedError{Code: RoutingRefusedProjection, Report: report, Cause: cause,
		Reason: fmt.Sprintf("the subscriptions could not be projected: %v", cause)}
}

// judgeSpineSwitch reads a projection that succeeded and returns why its lane
// still cannot route by the spine, or nil.
func judgeSpineSwitch(in SubscriptionInput, lane string, report SubscriptionReport) *RoutingRefusedError {
	// Overlaps is a refusal even from a projector that reported it without
	// an error: two rows of one bot must never both match.
	if len(report.Overlaps) > 0 {
		return spineProjectionRefusal(report, nil)
	}
	if len(report.Enabled[lane]) == 0 {
		reason := fmt.Sprintf("the projection enabled no row on the %s lane, so every delivery would be filtered", lane)
		if len(report.Disabled) > 0 {
			bots := make([]string, 0, len(report.Disabled))
			for bot := range report.Disabled {
				bots = append(bots, bot)
			}
			sort.Strings(bots)
			why := make([]string, 0, len(bots))
			for _, bot := range bots {
				why = append(why, bot+": "+report.Disabled[bot])
			}
			reason += " — " + strings.Join(why, "; ")
		}
		return &RoutingRefusedError{Code: RoutingRefusedNoEnabledRow, Report: report, Reason: reason}
	}
	if keys := launchVarsDiff(in.Config.LaunchVars, manifestLaunchVars(in.Bots, in.FrByBot)); len(keys) > 0 {
		return &RoutingRefusedError{Code: RoutingRefusedHandPinnedLaunchVars, Report: report,
			Reason: fmt.Sprintf("launch_vars key(s) [%s] on webhook %s differ from what the bots' manifests declare, and a spine launch does not apply that map: pin them on the integration's launch_vars (the repo-bots update), which rewrites the webhook's copy from the manifests, then switch again",
				strings.Join(keys, " "), in.Config.ID)}
	}
	return nil
}

// launchVarsDiff returns, sorted, every key on which two launch-var maps
// disagree: held by one alone, or by both with different values.
func launchVarsDiff(a, b map[string]string) []string {
	var keys []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			keys = append(keys, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
