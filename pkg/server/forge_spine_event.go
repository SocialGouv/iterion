package server

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// envForgeRoutingDefault is the server-wide routing default ("issues=legacy"
// form), parsed once when the server is built (parseForgeRoutingDefault); a
// value it refuses fails the server's start.
const envForgeRoutingDefault = "ITERION_FORGE_ROUTING_DEFAULT"

// forgeIssueDelivery is the provider-normalized issue event the spine router
// consumes (ADR-172, plan forge_spine_event.go): everything both providers
// carry on an issues delivery, flattened once so the router and its var
// builders never re-read a provider payload. Added is the added-label list in
// payload order, filtered to the labels that pass the repository allowlist.
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

// githubIssueDelivery flattens a parsed GitHub issues event. The payload
// carries one added label per event; it joins Added only when it passes the
// repository allowlist — the same gate the handler ran before routing here.
func githubIssueDelivery(cfg webhooks.Config, p prforge.ParsedIssue) forgeIssueDelivery {
	d := forgeIssueDelivery{
		Provider:    webhooks.ProviderGitHub,
		ProjectPath: p.ProjectPath,
		Number:      int(p.IssueNumber),
		Title:       p.IssueTitle,
		Body:        p.IssueBody,
		URL:         p.IssueURL,
		State:       p.IssueState,
		Labels:      p.IssueLabels,
		CloneURL:    p.CloneURL,
		Sender:      p.SenderLogin,
	}
	if p.LabelName != "" && webhooks.MatchLabel(cfg.LabelAllowlist, p.LabelName) {
		d.Added = []string{p.LabelName}
	}
	return d
}

// gitlabIssueDelivery flattens a parsed GitLab issue hook. One event can add
// several labels: Added keeps the payload's own order, narrowed to the labels
// the repository allowlist admits.
func gitlabIssueDelivery(cfg webhooks.Config, p gitlab.ParsedIssue) forgeIssueDelivery {
	d := forgeIssueDelivery{
		Provider:    webhooks.ProviderGitLab,
		ProjectPath: p.ProjectPath,
		ProjectID:   p.ProjectID,
		Number:      int(p.IssueIID),
		Title:       p.Title,
		Body:        p.Description,
		URL:         p.URL,
		State:       p.State,
		Labels:      p.Labels,
		CloneURL:    p.CloneURL,
		RepoRef:     p.DefaultBranch,
		Sender:      p.AuthorUsername,
	}
	for _, label := range p.AddedLabels {
		if webhooks.MatchLabel(cfg.LabelAllowlist, label) {
			d.Added = append(d.Added, label)
		}
	}
	return d
}

// event builds the trigger.Event the forge router matches against the
// integration's projected subscriptions: Source=forge,
// Kind=bundle.ForgeEventIssueLabeled, Action=trigger.ForgeIssueAction,
// AddedLabels=Added, Payload{forge_kind:"issues"}, and an ID of
// "forge:<provider>:<binding>:<laneEvent.key()>" — the forge's delivery id,
// or the body hash when the forge sends none. The ID holds no run id.
func (d forgeIssueDelivery) event(cfg webhooks.Config, meta webhookEventMeta) trigger.Event {
	return trigger.Event{
		ID:       "forge:" + string(d.Provider) + ":" + forgeBindingOf(cfg) + ":" + laneEvent{DeliveryID: meta.DeliveryID}.key(),
		Source:   trigger.SourceForge,
		Kind:     bundle.ForgeEventIssueLabeled,
		Action:   trigger.ForgeIssueAction,
		TenantID: cfg.TenantID,
		Repo:     d.ProjectPath,
		Subject: trigger.Subject{
			Type:  "issue",
			ID:    meta.SubjectID,
			URL:   d.URL,
			Title: d.Title,
			Body:  d.Body,
			State: d.State,
		},
		Actor:       d.Sender,
		Labels:      d.Labels,
		AddedLabels: d.Added,
		Payload:     map[string]any{"forge_kind": "issues"},
		OccurredAt:  time.Now().UTC(),
	}
}

// legacyBaseString renders the legacy idempotency base of one trigger — the
// exact pre-hash strings webhooks_github.go's and webhooks_gitlab.go's issue
// lanes key on — so a spine target's bot-scoped chains and a rollback's reads
// derive from the same bytes legacy hashed. GitHub carries no branch: the
// ref half of the key is the empty string, as the handler has always written.
func (d forgeIssueDelivery) legacyBaseString(cfg webhooks.Config, trigger string) string {
	if d.Provider == webhooks.ProviderGitLab {
		return fmt.Sprintf("gl|issue|%s|%s|%d|%d|%s", cfg.TenantID, cfg.ID, d.ProjectID, d.Number, trigger)
	}
	return fmt.Sprintf("gh|issue|%s|%s|%s|%d|%s", cfg.TenantID, cfg.ID, d.ProjectPath, d.Number, trigger)
}

// legacyTrigger names the trigger legacy used for THIS event: the added
// label on GitHub (the payload carries one), GitLab's
// webhooks.FirstMatchingLabel in payload order. It is read only for this
// event's identity — the smallest matching label, not this, is what a spine
// target claims its lane under.
func (d forgeIssueDelivery) legacyTrigger(cfg webhooks.Config) string {
	if d.Provider == webhooks.ProviderGitLab {
		return webhooks.FirstMatchingLabel(cfg.LabelAllowlist, d.Added)
	}
	if len(d.Added) == 0 {
		return ""
	}
	return d.Added[0]
}

// legacyString renders the legacy lane's base, hashed: the chain every
// legacy-era claim of this trigger sits on, which a spine target reads
// (serving and absorption) beside its own bot-scoped chain.
func (d forgeIssueDelivery) legacyString(cfg webhooks.Config, trigger string) string {
	return knowledge.ChecksumHex([]byte(d.legacyBaseString(cfg, trigger)))
}

// ErrForgeRoutingDefault is the refusal of an ITERION_FORGE_ROUTING_DEFAULT
// value the server does not start with.
var ErrForgeRoutingDefault = errors.New("server: invalid " + envForgeRoutingDefault)

// parseForgeRoutingDefault parses ITERION_FORGE_ROUTING_DEFAULT: comma-
// separated lane=mode pairs ("issues=legacy"), each lane at most once, lane
// and mode spelled exactly. Only legacy is a server default in this release:
// shadow parses and is refused as not shipped, and spine is refused because
// a lane routed by the spine needs what only a per-integration flip writes —
// the projected rows' SpineSince and the switch the routing guards read
// (PUT /api/teams/{id}/forge/repo-bots/{integration_id}/routing). Blank is
// no default at all. Every refusal wraps ErrForgeRoutingDefault.
func parseForgeRoutingDefault(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		name, mode, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found {
			return nil, fmt.Errorf("%w: %q is not a lane=mode pair", ErrForgeRoutingDefault, pair)
		}
		switch name {
		case webhooks.LaneIssues, webhooks.LanePullRequests, webhooks.LaneComments:
		default:
			return nil, fmt.Errorf("%w: unknown lane %q (want %s, %s or %s)", ErrForgeRoutingDefault, name,
				webhooks.LaneIssues, webhooks.LanePullRequests, webhooks.LaneComments)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: lane %q is named twice", ErrForgeRoutingDefault, name)
		}
		switch mode {
		case webhooks.RoutingLegacy:
		case webhooks.RoutingShadow:
			return nil, fmt.Errorf("%w: %s=%s: shadow routing is not shipped in this release", ErrForgeRoutingDefault, name, mode)
		case webhooks.RoutingSpine:
			return nil, fmt.Errorf("%w: %s=%s: spine is not a server default — flip each integration with PUT /api/teams/{id}/forge/repo-bots/{integration_id}/routing", ErrForgeRoutingDefault, name, mode)
		default:
			return nil, fmt.Errorf("%w: lane %q: unknown mode %q (want %s)", ErrForgeRoutingDefault, name, mode, webhooks.RoutingLegacy)
		}
		out[name] = mode
	}
	return out, nil
}
