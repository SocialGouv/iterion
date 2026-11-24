package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// connectorSubmitter launches what a matched connector pair owes
// (intake.Submitter), as the idempotent submission the pair's key names.
//
// The launch reads the subscription as it stands NOW, never as it stood at
// the match (ADR-168 Q2): a subscription deleted, disabled, re-bound to
// another connection or trigger, or whose filters and `when` no longer
// accept the event, refuses the pair; the vars are computed from it then.
// A repo_url the event names is believed only on the connection's own host
// or one the ingress allows (Q7). Every refusal is permanent and named; an
// outage is an error the worker retries; the launcher's own classes — a
// spent submission, a transient gate denial (Q3) — pass through as they are.
type connectorSubmitter struct {
	subs     trigger.SubscriptionStore
	matcher  *binding.Matcher
	launcher trigger.Launcher
	// botVars lists the bot's declared var names; found is false for a bot
	// the tenant's catalog does not hold.
	botVars func(ctx context.Context, tenantID, botID string) (names []string, found bool, err error)
	// repoTrusted judges a repo_url host against the intake's connection
	// and ingress: a refusal error, or an outage.
	repoTrusted func(ctx context.Context, in intake.Intake, host string) error
}

var _ intake.Submitter = (*connectorSubmitter)(nil)

func (c *connectorSubmitter) Submit(ctx context.Context, in intake.Intake, p intake.Pair) (string, error) {
	sub, err := c.subs.Get(ctx, p.SubscriptionID)
	switch {
	case errors.Is(err, trigger.ErrSubscriptionNotFound):
		return "", intake.Refuse("its subscription was deleted", nil)
	case err != nil:
		return "", fmt.Errorf("read subscription %s: %w", p.SubscriptionID, err)
	}
	if sub.TenantID != binding.SubscriptionTenant(in.TenantID) {
		return "", intake.Refuse("its subscription belongs to another tenant", nil)
	}
	if !sub.Enabled {
		return "", intake.Refuse("its subscription is disabled", nil)
	}
	if sub.ConnectorKey != trigger.ConnectorKey(in.ConnectionID, in.TriggerID) || sub.Poller != in.PollerID {
		return "", intake.Refuse("its subscription now listens to another connection, trigger or poller", nil)
	}
	mode, err := sub.EffectiveMode()
	if err != nil {
		return "", intake.Refuse(err.Error(), nil)
	}
	if mode != bundle.ExecutionDirect {
		return "", intake.Refuse(fmt.Sprintf("its subscription's mode %q launches no connector run", mode), nil)
	}
	v, ev, err := c.matcher.Rejudge(ctx, in, sub)
	if err != nil {
		return "", err
	}
	if v.Refusal != "" {
		return "", intake.Refuse(v.Refusal, nil)
	}
	if !v.Matched {
		return "", intake.Refuse("its subscription no longer matches the event: "+v.Reason, nil)
	}
	names, found, err := c.botVars(ctx, sub.TenantID, sub.BotID)
	if err != nil {
		return "", fmt.Errorf("read bot %s: %w", sub.BotID, err)
	}
	if !found {
		return "", intake.Refuse(fmt.Sprintf("bot %q is not in the tenant's catalog", sub.BotID), nil)
	}
	vars, refusal, err := binding.Vars(ctx, sub, ev, names)
	if err != nil {
		return "", err
	}
	if refusal != "" {
		return "", intake.Refuse(refusal, nil)
	}
	repoURL, _ := binding.LaunchString(ev.Outputs["repo_url"])
	repoRef, _ := binding.LaunchString(ev.Outputs["repo_ref"])
	if repoURL != "" {
		u, perr := url.Parse(repoURL)
		if perr != nil || u.Hostname() == "" {
			return "", intake.Refuse(fmt.Sprintf("the event's repo_url %q is not a URL", repoURL), nil)
		}
		if err := c.repoTrusted(ctx, in, strings.ToLower(u.Hostname())); err != nil {
			return "", err
		}
	}
	return c.launcher.Launch(ctx, trigger.LaunchPlan{
		BotID:           sub.BotID,
		TenantID:        sub.TenantID,
		Repo:            sub.Repo,
		Mode:            mode,
		Vars:            vars,
		KeyOverrides:    sub.KeyOverrides,
		SecretOverrides: sub.SecretOverrides,
		RepoURL:         repoURL,
		RepoRef:         repoRef,
		SourceRef: &store.RunSource{
			Kind:       store.RunSourceKindConnector,
			ScheduleID: sub.ID,
			PairID:     p.ID,
			IntakeID:   in.ID,
		},
		Retry:         sub.RetryPolicy(),
		Routing:       sub.RoutingPolicy(),
		SubmissionKey: p.SubmissionKey(),
	})
}

// connectorSubmitter wires the submitter over the server's stores.
func (s *Server) connectorSubmitter() *connectorSubmitter {
	return &connectorSubmitter{
		subs:     s.cfg.TriggerStore,
		matcher:  s.connectorMatcher(),
		launcher: s.triggerLauncher(),
		botVars: func(ctx context.Context, tenantID, botID string) ([]string, bool, error) {
			// The team tier is read strictly — a store outage is a retried
			// attempt, never a permanent refusal — but it exists only for a
			// real tenant, wired: the local scope (tenant "") answers from
			// the platform tier alone, and a bot absent from every tier is
			// the clean absence the pair refuses on.
			if tenantID != "" && s.botSources != nil {
				if entry, found, err := s.storedBotEntry(ctx, tenantID, botID); err != nil {
					return nil, false, err
				} else if found {
					return botVarNames(entry), true, nil
				}
			}
			entry, found, err := s.effectiveFindByNameForTeam(ctx, tenantID, botID)
			if err != nil || !found {
				return nil, found, err
			}
			return botVarNames(entry), true, nil
		},
		repoTrusted: s.connectorRepoTrusted,
	}
}

// botVarNames lists the var names a bot's schema declares.
func botVarNames(entry botregistry.EntryWithSchema) []string {
	var names []string
	if entry.Vars != nil {
		for _, f := range entry.Vars.Fields {
			names = append(names, f.Name)
		}
	}
	return names
}

// connectorMatcher is the matcher over the server's subscription store and
// its tenant catalogs.
func (s *Server) connectorMatcher() *binding.Matcher {
	return &binding.Matcher{Subscriptions: s.cfg.TriggerStore, Catalog: s.connectorCatalogFor}
}

// connectorRepoTrusted believes a repo_url host when it is the host of the
// intake's connection — its pinned base URL, else its package's default —
// or one its ingress lists in repo_hosts (ADR-168 §5).
func (s *Server) connectorRepoTrusted(ctx context.Context, in intake.Intake, host string) error {
	if s.connectionStore == nil {
		return errors.New("no connection plane to judge the event's repo_url against")
	}
	conn, err := s.connectionStore.Get(ctx, in.TenantID, in.ConnectionID)
	switch {
	case errors.Is(err, connection.ErrNotFound):
		return intake.Refuse(fmt.Sprintf("its connection %s is gone", in.ConnectionID), nil)
	case err != nil:
		return fmt.Errorf("read connection %s: %w", in.ConnectionID, err)
	}
	base := conn.BaseURL
	if base == "" {
		pkg, _, err := s.connectorCatalogFor(in.TenantID).Package(conn.Connector)
		switch {
		case connection.IsCatalogUnavailable(err):
			return err
		case err == nil:
			base = pkg.Connector.BaseURL.Default
		}
	}
	own := ""
	if u, err := url.Parse(base); err == nil {
		own = strings.ToLower(u.Hostname())
	}
	if own != "" && host == own {
		return nil
	}
	if in.IngressID != "" && s.webhookConfigs != nil {
		// The ingress id is the intake's own; read under the intake's tenant,
		// another tenant's ingress answers as an absent one.
		cfg, err := s.readWebhookForTenant(ctx, in.TenantID, in.IngressID)
		switch {
		case errors.Is(err, webhooks.ErrNotFound):
		case err != nil:
			return fmt.Errorf("read ingress %s: %w", in.IngressID, err)
		case slices.ContainsFunc(cfg.RepoHosts, func(h string) bool { return strings.EqualFold(strings.TrimSpace(h), host) }):
			return nil
		}
	}
	return intake.Refuse(fmt.Sprintf("the event's repo_url host %q is neither its connection's (%q) nor in its ingress's repo_hosts", host, own), nil)
}
