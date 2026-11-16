package server

// The source catalog read model (#3272): what an operator could plug, as
// data — the connector packages that declare triggers across the tiers
// that serve this tenant, plus the built-in families the studio already
// has dialogs for. A read model over the catalog, the connections store
// and the trigger store (philosophy §5): it computes nothing a store
// does not already know, and counts nothing outside the tenant that asks.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
)

// sourceEntry is one catalog card.
type sourceEntry struct {
	Kind          string             `json:"kind"` // "connector" | "builtin"
	ID            string             `json:"id"`
	DisplayName   string             `json:"display_name"`
	Description   string             `json:"description,omitempty"`
	Category      string             `json:"category,omitempty"`
	Maturity      string             `json:"maturity,omitempty"`
	Tier          string             `json:"tier,omitempty"`
	Delivery      string             `json:"delivery,omitempty"` // push; poll arrives with the polling lane
	Auth          []sourceAuthScheme `json:"auth,omitempty"`
	Triggers      []sourceTrigger    `json:"triggers,omitempty"`
	Connections   int                `json:"connections"`
	Subscriptions int                `json:"subscriptions"`
	// Ingress is the package's delivery contract, summarized for the
	// consent UI: where the credential travels, who mints the secret,
	// and whether deliveries name an installation. Nil for a package
	// whose triggers declare no ingress (the legacy forge lane).
	Ingress *sourceIngress `json:"ingress,omitempty"`
}

// sourceIngress mirrors a package's ingress block, read-only.
type sourceIngress struct {
	CredentialScheme     string `json:"credential_scheme"`
	SecretOrigin         string `json:"secret_origin"` // minted | vendor
	EventHeader          string `json:"event_header,omitempty"`
	InstallationRequired bool   `json:"installation_required"`
}

type sourceAuthScheme struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name,omitempty"`
}

type sourceTrigger struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	// Outputs and Filters are what the wizard's mapping and narrowing
	// steps render: the named values a delivery carries and the values an
	// operator may filter on, exactly as the package declares them.
	Outputs []sourceOutput `json:"outputs,omitempty"`
	Filters []sourceFilter `json:"filters,omitempty"`
}

type sourceOutput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
}

type sourceFilter struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
	Default []string `json:"default,omitempty"`
}

// builtinSources are the families that predate connector packages: the
// studio routes their cards to the dialogs they already have (schedules
// tab, the board selector, run completion, /triggers/emit).
var builtinSources = []sourceEntry{
	{Kind: "builtin", ID: "schedule", DisplayName: "Schedule", Description: "Fires a bot on a cron or interval, from the schedules tab."},
	{Kind: "builtin", ID: "board", DisplayName: "Board event", Description: "Fires when a native-board card enters a state, from the board selector."},
	{Kind: "builtin", ID: "run_outcome", DisplayName: "Run outcome", Description: "Fires when a run completes or fails."},
	{Kind: "builtin", ID: "emit", DisplayName: "Emitted event", Description: "Fires on an event another bot posts to /api/v1/triggers/emit."},
}

func (s *Server) registerSourcesRoutes() {
	s.mux.Handle("GET /api/v1/sources", s.requireAuth(http.HandlerFunc(s.handleListSources)))
}

// handleListSources serves the active team's view; the team-scoped twin
// (teamTriggerAuth's family) serves the path team's.
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	s.writeSources(w, r, s.triggerTenant(r))
}

// writeSources assembles the read model for one tenant: catalog entries
// that declare triggers, then the built-ins.
func (s *Server) writeSources(w http.ResponseWriter, r *http.Request, tenant string) {
	cat := s.connectorCatalogFor(tenant)
	if cat == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "the connector catalog is not wired on this deployment")
		return
	}
	entries, err := s.connectorSources(r.Context(), cat, tenant)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "list the connector catalog: %v", err)
		return
	}
	entries = append(entries, builtinSources...)
	dispatcher.WriteJSON(w, http.StatusOK, map[string]any{"sources": entries})
}

// connectorSources builds one card per package that declares triggers,
// with the tier that served it and this tenant's counts.
func (s *Server) connectorSources(ctx context.Context, cat connection.Catalog, tenant string) ([]sourceEntry, error) {
	ids, err := cat.Connectors()
	if err != nil {
		return nil, err
	}
	connsByConnector, err := s.connectionsByConnector(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	subsByConnector, err := s.subscriptionsByConnector(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	var out []sourceEntry
	for _, id := range ids {
		pkg, tier, err := cat.Package(id)
		if connection.IsNoSuchConnector(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// A package with no inbound half is an outbound tool, not a
		// source: it has nothing to plug.
		if len(pkg.Triggers) == 0 {
			continue
		}
		e := sourceEntry{
			Kind:          "connector",
			ID:            id,
			DisplayName:   pkg.Connector.DisplayName,
			Description:   pkg.Connector.Description,
			Category:      string(pkg.Connector.Category),
			Maturity:      string(pkg.Connector.Maturity),
			Tier:          string(tier.Kind),
			Delivery:      "push",
			Connections:   connsByConnector[id],
			Subscriptions: subsByConnector[id],
		}
		for _, a := range pkg.Connector.Auth {
			e.Auth = append(e.Auth, sourceAuthScheme{ID: a.ID, Kind: string(a.Kind), DisplayName: a.DisplayName})
		}
		if pkg.Ingress != nil {
			ing := &sourceIngress{
				CredentialScheme: pkg.Ingress.Credential.Scheme,
				SecretOrigin:     pkg.Ingress.Credential.Secret,
				EventHeader:      pkg.Ingress.Event.Header,
			}
			if pkg.Ingress.Installation != "" {
				ing.InstallationRequired = true
			}
			e.Ingress = ing
		}
		for _, tr := range pkg.Triggers {
			st := sourceTrigger{ID: tr.ID, DisplayName: tr.DisplayName, Description: tr.Description}
			for _, o := range tr.Outputs {
				st.Outputs = append(st.Outputs, sourceOutput{Name: o.Name, Description: o.Description, Type: o.Type})
			}
			for _, f := range tr.Filters {
				st.Filters = append(st.Filters, sourceFilter{ID: f.ID, Type: f.Type, Options: f.Options, Default: f.Default})
			}
			e.Triggers = append(e.Triggers, st)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// connectionsByConnector counts the tenant's connections per connector in
// one listing.
func (s *Server) connectionsByConnector(ctx context.Context, tenant string) (map[string]int, error) {
	out := map[string]int{}
	if s.connectionStore == nil {
		return out, nil
	}
	conns, err := s.connectionStore.List(ctx, tenant, "")
	if err != nil {
		// An outage is the caller's 503, not a silent zero: the count is
		// the card's "already connected" promise.
		return nil, err
	}
	for _, c := range conns {
		out[c.Connector]++
	}
	return out, nil
}

// subscriptionsByConnector counts the tenant's connector subscriptions per
// connector: a subscription's trigger id is addressed <connector>.…, the
// same rule an operation id obeys.
func (s *Server) subscriptionsByConnector(ctx context.Context, tenant string) (map[string]int, error) {
	out := map[string]int{}
	if s.cfg.TriggerStore == nil {
		return out, nil
	}
	subs, err := s.cfg.TriggerStore.ListByTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if sub.ConnectionID == "" {
			continue
		}
		id, _, _ := strings.Cut(sub.TriggerID, ".")
		out[id]++
	}
	return out, nil
}

// handleTeamListSources is the team-scoped twin: the path team's view,
// readable at the same rung as the connections list.
func (s *Server) handleTeamListSources(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	s.writeSources(w, r, teamID)
}
