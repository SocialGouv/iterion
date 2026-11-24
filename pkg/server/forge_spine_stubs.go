package server

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// forgeSpineStubs marks this file as the P1a stub set (ADR-172): every shell
// below is a P1a stub the router lot implements. It compiles, routes nothing,
// and answers the zero value; the package-level const is deleted together
// with the file when the real forge_spine_event.go / forge_spine_route.go
// land. The witnesses in forge_spine_issue_test.go and
// forge_spine_event_test.go are written against these shapes and red until
// then.
//
//nolint:unused // the P1a stub set: used from the router commit on.
const forgeSpineStubs = true

// envForgeRoutingDefault is the server-wide routing default, read once per
// delivery through forgeRoutingDefaultFor ("issues=legacy" form).
const envForgeRoutingDefault = "ITERION_FORGE_ROUTING_DEFAULT"

// forgeIssueDelivery is the provider-normalized issue event the spine router
// consumes (plan §2, forge_spine_event.go): everything both providers carry
// on an issues delivery, flattened once so the router and its var builders
// never re-read a provider payload. Added is the added-label list in payload
// order, filtered to the labels that pass the repository allowlist.
type forgeIssueDelivery struct {
	Provider                webhooks.Provider
	ProjectPath             string
	ProjectID               int64 // GitLab
	Number                  int
	Title, Body, URL, State string
	// Labels is the issue's current label set; Added is what this event
	// added and the repository pre-gate admits.
	Labels                    []string
	Added                     []string
	CloneURL, RepoRef, Sender string
}

// githubIssueDelivery flattens a parsed GitHub issues event. P1a stub the
// router lot implements: the zero value until then.
func githubIssueDelivery(cfg webhooks.Config, p prforge.ParsedIssue) forgeIssueDelivery {
	return forgeIssueDelivery{}
}

// gitlabIssueDelivery flattens a parsed GitLab issue hook. P1a stub the
// router lot implements: the zero value until then.
func gitlabIssueDelivery(cfg webhooks.Config, p gitlab.ParsedIssue) forgeIssueDelivery {
	return forgeIssueDelivery{}
}

// event builds the trigger.Event the forge router matches against the
// integration's projected subscriptions: Source=forge,
// Kind=bundle.ForgeEventIssueLabeled, Action="labeled",
// AddedLabels=Added, Payload{forge_kind:"issues"}, and an ID of
// "forge:<provider>:<binding>:<laneEvent.key()>". P1a stub the router lot
// implements: the zero Event until then.
func (d forgeIssueDelivery) event(cfg webhooks.Config, meta webhookEventMeta) trigger.Event {
	return trigger.Event{}
}

// legacyTrigger names the trigger legacy used for THIS event: the added
// label on GitHub (the payload carries one), GitLab's
// webhooks.FirstMatchingLabel in payload order. P1a stub the router lot
// implements: "" until then.
func (d forgeIssueDelivery) legacyTrigger(cfg webhooks.Config) string {
	return ""
}

// legacyString renders the legacy idempotency base of one trigger — the
// exact strings webhooks_github.go's and webhooks_gitlab.go's issue lanes
// key on — so a spine target shares legacy's chains, and a rollback reads
// them. P1a stub the router lot implements: "" until then.
func (d forgeIssueDelivery) legacyString(cfg webhooks.Config, trigger string) string {
	return ""
}

// forgeRoutingFor resolves the routing mode of one lane for cfg:
// cfg.ForgeRouting.For(lane), else the server default
// (ITERION_FORGE_ROUTING_DEFAULT, "issues=legacy" form). P1a stub the router
// lot implements: "" until then, so no lane ever routes by spine.
//
//nolint:unused // the P1a stub set: used from the router commit on.
func (s *Server) forgeRoutingFor(cfg webhooks.Config, lane string) string {
	return ""
}

// forgeBindingOf names the forge binding the delivery's config projects its
// subscriptions under: the integration binding when the config carries a
// ProvisionIntegrationID, else the hand-created webhook's own binding. P1a
// stub the router lot implements: "" until then.
//
//nolint:unused // the P1a stub set: used from the router commit on.
func forgeBindingOf(cfg webhooks.Config) string {
	return ""
}

// routeIssueThroughSpine is the spine router (plan §2 steps 1-10): match the
// delivery against the binding's projected subscriptions, read every
// matched bot's lane in both stores, claim in sorted order, and answer
// through the webhook tail. P1a stub the router lot implements: the
// witnesses never reach it — the handler branches that would call it do not
// exist yet — so it answers nothing and writes this refusal, never a route.
//
//nolint:unused // the P1a stub set: used from the router commit on.
func (s *Server) routeIssueThroughSpine(ctx context.Context, w http.ResponseWriter, r *http.Request,
	cfg webhooks.Config, meta webhookEventMeta, d forgeIssueDelivery, payloadHash, srcIP string) {
	writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"status": "stub"})
}

// forgeRoutingDefaultFor reads the server-wide default routing mode of lane
// from ITERION_FORGE_ROUTING_DEFAULT ("issues=legacy" form, comma-separated
// lane=mode pairs). "" when unset or the lane names none. The reader is the
// piece the router lot wires into forgeRoutingFor; it decides nothing on its
// own.
func forgeRoutingDefaultFor(lane string) string {
	raw := strings.TrimSpace(os.Getenv(envForgeRoutingDefault))
	for _, pair := range strings.Split(raw, ",") {
		name, mode, found := strings.Cut(pair, "=")
		if !found || strings.TrimSpace(name) != lane {
			continue
		}
		return strings.TrimSpace(mode)
	}
	return ""
}
