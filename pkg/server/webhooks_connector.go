package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The connector webhook lane (#2761, ADR-098): one inbound delivery believed
// through the CONNECTION the webhook config is bound with, matched against
// the events the connector PACKAGE declares — no engine switch decides either
// half, which is the seam the second connector implements.
//
// The lane's contract, in order:
//  1. the binding resolves — the connection is read tenant-scoped, in the
//     store's signature (a config naming another tenant's connection reads
//     not-found, never the record);
//  2. the connection may be BELIEVED — it names this webhook's connector and
//     holds the `trigger` grant, the consent an operator gave when entrusting
//     it (refusal is a typed *connection.CapabilityError);
//  3. the event is DECLARED — the package's triggers.yaml carries the
//     vendor's event name, or the delivery is filtered with the package's own
//     answer;
//  2b. the connection is LIVE — a revoked or needs-reauth connection vouches
//     for nothing (the same refusal the action/agent chokepoint applies);
//  3b. the webhook's own narrowings still apply — event allowlist, project
//     and author allowlists, the hold-label veto (this is an auto-launch
//     lane);
//  4. only then does the delivery dispatch, through the same tail every
//     webhook lane shares (idempotency, quota, delivery record).
//
// The signature gate is NOT here: it runs before this lane is entered, in the
// provider handler, because the middleware's contract is VerifyHMACSignature
// over the raw body BEFORE any side effect — and every refusal below records
// a terminal delivery, which is one.

// connectorDeliveryID returns the vendor's own delivery id, accepting both
// header spellings (Forgejo and its Gitea-compatible twin, like the event and
// signature headers). Empty when the vendor sent none — the caller falls back
// to the payload hash, which dedupes an exact retransmission just as well.
func connectorDeliveryID(r *http.Request) string {
	if v := r.Header.Get("X-Forgejo-Delivery"); v != "" {
		return v
	}
	return r.Header.Get("X-Gitea-Delivery")
}

// forgejoIssueComment is the one declared event's payload, parsed to exactly
// the fields the dispatch names — the whole body is already pinned by
// payloadHash, and a field nobody reads is a var nobody can trust.
type forgejoIssueComment struct {
	Action string `json:"action"`
	Issue  struct {
		Number int64 `json:"number"`
		// Labels carry the issue's label set for the hold-label veto.
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue"`
	Comment struct {
		ID int64 `json:"id"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// LabelNames flattens the issue's labels to their names, the shape the
// hold-label veto reads.
func (p forgejoIssueComment) LabelNames() []string {
	if len(p.Issue.Labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Issue.Labels))
	for _, l := range p.Issue.Labels {
		out = append(out, l.Name)
	}
	return out
}

// handleConnectorDelivery runs the connector lane for a delivery whose
// signature already verified. Every refusal is permanent (the binding, the
// grant or the package — none heals by the vendor retrying), so each answers
// 200 with status filtered and records the reason, like every other filtered
// verdict on these lanes: a 4xx/5xx would only teach the forge to disable
// the hook.
func (s *Server) handleConnectorDelivery(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP, event string) {
	meta := webhookEventMeta{Kind: event}
	filter := func(reason string) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		if s.logger != nil {
			s.logger.Warn("webhooks: connector delivery for %s filtered: %s", cfg.ID, reason)
		}
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "error": reason})
	}

	if s.connectionStore == nil || s.connectorCatalog == nil {
		// A binding nothing can resolve is a deployment defect, not a
		// verdict: the field is only writable where the plane exists.
		httpError(w, http.StatusServiceUnavailable, "webhook %s is bound to a connection but this deployment has no connection plane", cfg.ID)
		return
	}

	// 1. The binding resolves, tenant-scoped: the tenant is the store's
	// first argument, so a config naming another tenant's connection is a
	// not-found here — the check cannot be forgotten at this call site.
	conn, err := s.connectionStore.Get(ctx, cfg.TenantID, cfg.ConnectorConnectionID)
	if err != nil {
		filter(fmt.Sprintf("the connection this webhook is bound to (%s) no longer exists — re-bind or recreate it", cfg.ConnectorConnectionID))
		return
	}
	// 2. The connection may be believed, and for THIS connector: a webhook
	// bound across connectors would vouch a delivery with a credential an
	// operator granted to a different vendor entirely.
	if conn.Connector != string(cfg.Provider) {
		filter(fmt.Sprintf("the bound connection is a %q connection; this webhook receives %q deliveries — re-bind it to a %s connection",
			conn.Connector, cfg.Provider, cfg.Provider))
		return
	}

	if !conn.Has(connection.CapTrigger) {
		capErr := &connection.CapabilityError{Alias: conn.Alias, Want: connection.CapTrigger, Have: conn.Capabilities}
		filter(capErr.Error())
		return
	}
	// A REVOKED (or otherwise unusable) connection vouches for nothing: the
	// trigger grant rides on the record being live, exactly as the action/agent
	// chokepoint refuses a non-usable connection (resolve.go). Revocation is
	// the incident-response tool — a lane that never reads Status leaves it
	// inoperant for inbound deliveries.
	if !conn.Status.Usable() {
		filter(fmt.Sprintf("the bound connection %q (%s) is %s — a revoked or reauth-pending connection vouches for no delivery; reconnect or re-bind",
			conn.Alias, conn.ID, conn.Status))
		return
	}

	// 3. The event is declared by the PACKAGE, not by an engine switch: a
	// connector's second event costs a triggers.yaml line, not a code change.
	pkg, tier, err := s.connectorCatalog.Package(conn.Connector)
	if err != nil {
		filter(fmt.Sprintf("the %s cannot serve the bound connector %q: %v", tier, conn.Connector, err))
		return
	}
	var triggerID string
	for _, t := range pkg.Triggers {
		if t.Event == event {
			triggerID = t.ID
			break
		}
	}
	if triggerID == "" {
		filter(fmt.Sprintf("the %s package declares no %q event — it declares %s, and an engine may believe only what the package declares",
			conn.Connector, event, triggerEventNames(pkg.Triggers)))
		return
	}

	// The package's declaration is what MAY be believed; the webhook's own
	// event allowlist stays the operator's narrowing on top of it, exactly
	// like the legacy lanes. An empty allowlist allows what the lane exists
	// for (this lane is entered for one bound purpose, unlike the multi-lane
	// provider switch, whose defaults answer a different question).
	if !webhooks.MatchEvent(cfg.EventAllowlist, event, event) {
		filter(fmt.Sprintf("event %q is outside this webhook's event allowlist", event))
		return
	}

	// The declared event's payload. A body that will not parse is the
	// vendor's answer malformed, not a refusal: invalid, like every lane's.
	var pl forgejoIssueComment
	if err := json.Unmarshal(body, &pl); err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, "malformed "+event+" payload: "+err.Error())
		httpError(w, http.StatusBadRequest, "malformed %s payload: %v", event, err)
		return
	}
	meta.Action = pl.Action
	meta.ProjectPath = pl.Repository.FullName
	if pl.Comment.ID != 0 {
		meta.SubjectID = fmt.Sprintf("comment:%d", pl.Comment.ID)
		meta.ParentSubjectID = fmt.Sprintf("issue:%d", pl.Issue.Number)
	}

	// The operator's narrowings, exactly as the legacy lanes apply them: the
	// project allowlist (an org-level hook otherwise fires the bot on every
	// repo of the org) and the author allowlist (the delivery's sender is the
	// author of a comment event).
	if !webhooks.MatchProject(cfg.ProjectAllowlist, pl.Repository.FullName) {
		filter(fmt.Sprintf("project %q is outside this webhook's project allowlist", pl.Repository.FullName))
		return
	}
	if !webhooks.MatchAuthor(cfg.AuthorAllowlist, pl.Sender.Login) {
		filter(fmt.Sprintf("author %q is outside this webhook's author allowlist", pl.Sender.Login))
		return
	}
	// The hold-label veto: this is an AUTO-launch lane (a vendor event, not a
	// human command), so it honors the bot-agnostic pause like every other
	// auto-launch lane (webhooks_common.go).
	if s.suppressedByHoldLabel(ctx, w, cfg, meta, pl.LabelNames(), payloadHash, srcIP) {
		return
	}

	botID := cfg.SelectBot()
	if botID == "" {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, "bot required (set the webhook's default bot)")
		httpError(w, http.StatusBadRequest, "bot required (set the webhook's default bot)")
		return
	}
	if !cfg.AllowsBot(botID) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, "bot not permitted by webhook scope")
		httpError(w, http.StatusForbidden, "bot %q not permitted by this webhook", botID)
		return
	}

	dedup := connectorDeliveryID(r)
	if dedup == "" {
		dedup = payloadHash
	}
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("connector|%s|%s|%s", cfg.TenantID, cfg.ID, dedup)))

	vars := map[string]string{
		"connector_trigger": triggerID,
		"forgejo_event":     event,
		"forgejo_action":    pl.Action,
		"repo":              pl.Repository.FullName,
		"sender":            pl.Sender.Login,
	}
	if pl.Issue.Number != 0 {
		vars["issue_number"] = fmt.Sprint(pl.Issue.Number)
	}
	if pl.Comment.ID != 0 {
		vars["comment_id"] = fmt.Sprint(pl.Comment.ID)
	}
	applyWebhookVarLayers(vars, cfg)

	// Trusted, stated rather than defaulted: the delivery is believed on the
	// connection's word and the payload names only the repo to clone — the
	// same class as the generic lane, which names its own.
	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, botID, vars, pl.Repository.CloneURL, "", payloadHash, srcIP, launchProvenance{})
}

// validateConnectorBinding is the WRITE half of the lane's contract: a
// webhook may bind only a connection it could be believed through, checked
// when the operator entrusts the binding rather than discovered per
// delivery. Every refusal here is a 400 an operator can act on; the lane's
// own re-checks are for the record that changed since.
func (s *Server) validateConnectorBinding(ctx context.Context, teamID string, provider webhooks.Provider, connID string) error {
	if s.connectionStore == nil || s.connectorCatalog == nil {
		return fmt.Errorf("a connector binding needs the connection plane (connections store + catalog), which this deployment does not wire")
	}
	conn, err := s.connectionStore.Get(ctx, teamID, connID)
	if err != nil {
		return fmt.Errorf("connection %q not found for this team", connID)
	}
	if conn.Connector != string(provider) {
		return fmt.Errorf("connection %q (%s) is a %q connection; a %s webhook's deliveries can only be believed through a %s connection",
			connID, conn.Alias, conn.Connector, provider, provider)
	}
	if !conn.Has(connection.CapTrigger) {
		return fmt.Errorf("connection %q (%s) holds no `trigger` grant — an operator grants it deliberately (`iterion connections add --capabilities …,trigger`), which is what decides this vendor's events may be believed on its word",
			connID, conn.Alias)
	}
	if !conn.Status.Usable() {
		return fmt.Errorf("connection %q (%s) is %s — a revoked or reauth-pending connection vouches for no delivery; reconnect it first, then bind",
			connID, conn.Alias, conn.Status)
	}
	return nil
}

// triggerEventNames renders the declared vendor events for a refusal: the
// operator reads what the package DOES believe, which is the fix for an
// event it does not.
func triggerEventNames(triggers []spec.Trigger) string {
	if len(triggers) == 0 {
		return "no events at all"
	}
	names := make([]string, 0, len(triggers))
	for _, t := range triggers {
		names = append(names, t.Event)
	}
	return strings.Join(names, ", ")
}
