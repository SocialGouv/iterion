package trigger

import (
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The prefixes of a forge binding and of the origin of a projected forge
// subscription. A binding names whose deliveries a subscription answers; an
// origin names who projected the row and may rewrite or delete it.
const (
	forgeIntegrationBindingPrefix = "integration:"
	forgeWebhookBindingPrefix     = "webhook:"
	forgeOriginPrefix             = "forge:"
)

// ForgeIssueAction is the action of the event a forge router emits when a
// label is added to an issue, whatever the forge calls it.
const ForgeIssueAction = "labeled"

// ForgeBindingForIntegration is the binding of the subscriptions projected
// from a forge repository integration. Two integrations of one repository
// hold two bindings, so neither fires the other's bots.
func ForgeBindingForIntegration(id string) string { return forgeIntegrationBindingPrefix + id }

// ForgeBindingForWebhook is the binding of the subscriptions of a webhook
// created by hand, which no integration owns.
func ForgeBindingForWebhook(id string) string { return forgeWebhookBindingPrefix + id }

// ForgeOrigin is the Origin of the subscriptions projected from a forge
// repository integration: the rows its projector rewrites and deletes.
func ForgeOrigin(integrationID string) string { return forgeOriginPrefix + integrationID }

// DerivedForgeSubscriptionID is the stable id of the subscription one
// invocation of a bot derives on a forge integration: key is the
// invocation's routing key (bundle.ForgeRoutingInvocation.Key). The
// integration's origin fills the id's fifth field, which a manifest-derived
// row leaves empty and a connector row fills with a connection id, so the
// id is shared with neither.
func DerivedForgeSubscriptionID(tenantID, botID, key, integrationID string) string {
	return DerivedSubscriptionID(tenantID, botID, bundle.InvocationKindForge, key, ForgeOrigin(integrationID))
}

// checkForgeOrigin holds the invariant of a projected forge row: an origin
// of a forge integration requires the binding of that same integration. A
// row that kept the origin and lost the binding would be listed as a bus
// candidate and removed by a manifest reconciliation.
func (s Subscription) checkForgeOrigin() error {
	id, ok := strings.CutPrefix(s.Origin, forgeOriginPrefix)
	if !ok {
		return nil
	}
	if id == "" {
		return &InvalidError{SubscriptionID: s.ID, Field: "origin",
			Reason: fmt.Sprintf("origin %q names no forge integration", s.Origin)}
	}
	if want := ForgeBindingForIntegration(id); s.ForgeBinding != want {
		return &InvalidError{SubscriptionID: s.ID, Field: "forge_binding",
			Reason: fmt.Sprintf("origin %q requires forge_binding %q, got %q", s.Origin, want, s.ForgeBinding)}
	}
	return nil
}

// ForgeTarget is what a forge subscription is projected onto: the tenant,
// the repository, and the integration or the hand-created webhook whose
// deliveries it answers. IntegrationID wins when both ids are set.
type ForgeTarget struct {
	TenantID      string
	IntegrationID string
	WebhookID     string
	Repo          string
}

// binding returns the target's forge binding and the origin of the rows
// projected onto it; ok is false for a target that names neither an
// integration nor a webhook.
func (t ForgeTarget) binding() (binding, origin string, ok bool) {
	switch {
	case t.IntegrationID != "":
		return ForgeBindingForIntegration(t.IntegrationID), ForgeOrigin(t.IntegrationID), true
	case t.WebhookID != "":
		// A hand-created webhook's rows carry their binding as their
		// origin: no integration projects them.
		return ForgeBindingForWebhook(t.WebhookID), ForgeBindingForWebhook(t.WebhookID), true
	}
	return "", "", false
}

// FromForgeIssueInvocation derives the subscription that starts botID when
// a label is added to an issue of the target, from one routing invocation
// of the bot (bundle.ForgeIssueInvocations).
//
// labels are the patterns an added label must match (nil claims any) and
// exclude the ones it must not match (nil excludes none); vars are the
// launch vars the row stamps on the run; a row derived disabled keeps
// disabledReason, an enabled one carries none; digest is the digest of the
// inputs the caller planned the row from. The caller supplies id
// (DerivedForgeSubscriptionID for an integration's row).
//
// ok is false when no row may be written: the target names neither an
// integration nor a webhook — a row with no binding would be a bus
// subscription — the invocation is not a kind=forge issue_labeled one, or
// its mode is one no effect honours.
func FromForgeIssueInvocation(id string, t ForgeTarget, botID string, ri bundle.ForgeRoutingInvocation,
	labels, exclude []string, vars map[string]string, enabled bool, disabledReason, digest string, now time.Time) (Subscription, bool) {
	inv := ri.Inv
	if inv.Kind != bundle.InvocationKindForge || inv.Forge == nil || inv.Forge.Event != bundle.ForgeEventIssueLabeled {
		return Subscription{}, false
	}
	// A mode no effect honours (deliver until #3136) derives nothing: it
	// is never silently read as board or direct.
	mode, err := inv.EffectiveMode()
	if err != nil {
		return Subscription{}, false
	}
	binding, origin, ok := t.binding()
	if !ok {
		return Subscription{}, false
	}
	sub := baseSubscription(id, t.TenantID, t.Repo, botID, origin, bundle.InvocationKindForge, now)
	sub.Mode = mode
	sub.Match = Matcher{
		Sources:            []Source{SourceForge},
		Kinds:              []string{bundle.ForgeEventIssueLabeled},
		Actions:            []string{ForgeIssueAction},
		AddedLabels:        append([]string(nil), labels...),
		ExcludeAddedLabels: append([]string(nil), exclude...),
	}
	// A target with no repository (a group-wide webhook) leaves the
	// repository dimension off: a list holding "" would match only an
	// event that names no repository.
	if t.Repo != "" {
		sub.Match.Repos = []string{t.Repo}
	}
	sub.ArgsVar = ri.ArgsVar
	sub.OpensMR = ri.OpensMR
	sub.Vars = copyStrMap(vars)
	sub.ForgeBinding = binding
	sub.ProjectionDigest = digest
	sub.Enabled = enabled
	if !enabled {
		sub.DisabledReason = disabledReason
	}
	return sub, true
}
