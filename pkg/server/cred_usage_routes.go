package server

import (
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credusage"
)

// registerCredUsageRoutes wires the per-credential usage views (#641) —
// what the org bucket cannot answer: not "how much did this org consume"
// but "what did THIS key cost". Registered only when the counter is wired.
func (s *Server) registerCredUsageRoutes() {
	s.mux.Handle("GET /api/teams/{id}/credentials/usage", s.requireAuth(http.HandlerFunc(s.handleTeamCredentialUsage)))
	s.mux.Handle("GET /api/admin/credentials/usage", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminCredentialUsage)))
}

// credentialUsageView is one credential's month.
//
// nature sits BESIDE cost_usd on purpose. A subscription bills nothing per
// call, so its figure is what those calls WOULD have cost metered; an API
// key's is a charge on a real invoice. Two amounts of different nature must
// never be summed, and a client that only reads cost_usd would do exactly
// that — so the API states it rather than leaving it to a doc nobody opens.
type credentialUsageView struct {
	Month        string   `json:"month"`
	Fingerprint  string   `json:"fingerprint"`
	Provider     string   `json:"provider"`
	Tier         string   `json:"tier"`
	TenantID     string   `json:"tenant_id,omitempty"`
	Nature       string   `json:"nature"`
	CostUSD      float64  `json:"cost_usd"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Runs         int      `json:"runs"`
	Backends     []string `json:"backends,omitempty"`
}

// credentialUsageListView is the response envelope.
type credentialUsageListView struct {
	Month       string                `json:"month"`
	Credentials []credentialUsageView `json:"credentials"`
	// MeteredUSD / EstimatedUSD are the two totals, kept apart for the
	// reason nature exists: one is an invoice, the other is not.
	MeteredUSD   float64 `json:"metered_usd"`
	EstimatedUSD float64 `json:"estimated_usd"`
}

func toCredentialUsageList(month string, rows []credusage.MonthlyUsage) credentialUsageListView {
	out := credentialUsageListView{Month: month, Credentials: make([]credentialUsageView, 0, len(rows))}
	for _, r := range rows {
		out.Credentials = append(out.Credentials, credentialUsageView{
			Month: r.Month, Fingerprint: r.Fingerprint, Provider: r.Provider,
			Tier: string(r.Tier), TenantID: r.TenantID, Nature: string(r.Nature),
			CostUSD: r.CostUSD, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			Runs: r.Runs, Backends: r.Backends,
		})
		if r.Nature == credusage.NatureMetered {
			out.MeteredUSD += r.CostUSD
		} else {
			out.EstimatedUSD += r.CostUSD
		}
	}
	return out
}

// tierOrPlatform reads the admin route's optional `?tier=`; the platform
// tier is the default because it is the one no tenant view can show.
func tierOrPlatform(raw string) string {
	switch credusage.Tier(raw) {
	case credusage.TierTeam, credusage.TierPool, credusage.TierPlatform:
		return raw
	}
	return string(credusage.TierPlatform)
}

// handleTeamCredentialUsage lists what each credential cost THIS team this
// month. A platform or lent credential appears with the team's own slice of
// it — the whole of it is the admin view's answer.
func (s *Server) handleTeamCredentialUsage(w http.ResponseWriter, r *http.Request) {
	if s.credUsage == nil {
		httpError(w, http.StatusNotFound, "per-credential usage is not enabled on this instance")
		return
	}
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member of this team")
		return
	}
	now := time.Now().UTC()
	rows, err := s.credUsage.List(r.Context(), now, teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toCredentialUsageList(now.Format("2006-01"), rows))
}

// handleAdminCredentialUsage is the platform view: one credential across
// every tenant it served (`?fingerprint=`), or the platform tier's own
// month. Super-admin, because a fingerprint spans tenants and the answer
// names them.
func (s *Server) handleAdminCredentialUsage(w http.ResponseWriter, r *http.Request) {
	if s.credUsage == nil {
		httpError(w, http.StatusNotFound, "per-credential usage is not enabled on this instance")
		return
	}
	now := time.Now().UTC()
	var (
		rows []credusage.MonthlyUsage
		err  error
	)
	if fp := r.URL.Query().Get("fingerprint"); fp != "" {
		rows, err = s.credUsage.ListByFingerprint(r.Context(), now, fp)
	} else {
		// By TIER, not by tenant: a platform credential is metered under
		// each tenant it served, so no single tenant holds its month.
		rows, err = s.credUsage.ListByTier(r.Context(), now, credusage.Tier(tierOrPlatform(r.URL.Query().Get("tier"))))
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toCredentialUsageList(now.Format("2006-01"), rows))
}
