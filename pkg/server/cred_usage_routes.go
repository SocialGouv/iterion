package server

import (
	"fmt"
	"net/http"
	"strings"
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
	Month       string `json:"month"`
	Fingerprint string `json:"fingerprint"`
	Provider    string `json:"provider"`
	Tier        string `json:"tier"`
	TenantID    string `json:"tenant_id,omitempty"`
	// RepoID is the repository this row is attributed to, present ONLY on a
	// `?repo=` answer. Every other listing sums a credential's repositories
	// together, and naming one of them on a total would be a wrong answer
	// rather than a partial one.
	RepoID  string  `json:"repo_id,omitempty"`
	Nature  string  `json:"nature"`
	CostUSD float64 `json:"cost_usd"`
	// InputTokens / OutputTokens are a MEASURED split and stay zero when
	// none was observed; AggregateTokens holds a CLI delegate's
	// unsplittable total. A per-direction ratio is only meaningful when
	// the aggregate is zero (#992).
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	AggregateTokens int64    `json:"aggregate_tokens"`
	Runs            int      `json:"runs"`
	Backends        []string `json:"backends,omitempty"`
}

// credentialUsageScope names what a listing was filtered on.
//
// A listing that does not say what it left out reads as "everything", and a
// credential absent from it reads as a credential that spent nothing. The
// admin route's ABSENT `?tier=` answers for the platform tier alone, so a
// team forfait's spend — metered on a team row — is invisible there. A
// production probe read exactly that as a frozen meter and opened a defect
// against a counter that was recording normally (#1087).
type credentialUsageScope struct {
	// Tier is the tier filter the admin listing applied, including the one
	// it defaulted to on its own.
	Tier string `json:"tier,omitempty"`
	// Fingerprint / Repo name the narrower questions, each exclusive of Tier.
	Fingerprint string `json:"fingerprint,omitempty"`
	Repo        string `json:"repo,omitempty"`
	// TeamID is set by the team route, whose rows are one tenant's slice of
	// each credential rather than the whole of it.
	TeamID string `json:"team_id,omitempty"`
}

// credentialUsageListView is the response envelope.
type credentialUsageListView struct {
	Month string `json:"month"`
	// Scope is always present: an answer states the question it answers,
	// not only its result.
	Scope       credentialUsageScope  `json:"scope"`
	Credentials []credentialUsageView `json:"credentials"`
	// MeteredUSD / EstimatedUSD are the two totals, kept apart for the
	// reason nature exists: one is an invoice, the other is not.
	MeteredUSD   float64 `json:"metered_usd"`
	EstimatedUSD float64 `json:"estimated_usd"`
}

func toCredentialUsageList(month string, scope credentialUsageScope, rows []credusage.MonthlyUsage) credentialUsageListView {
	out := credentialUsageListView{Month: month, Scope: scope, Credentials: make([]credentialUsageView, 0, len(rows))}
	for _, r := range rows {
		out.Credentials = append(out.Credentials, credentialUsageView{
			Month: r.Month, Fingerprint: r.Fingerprint, Provider: r.Provider,
			Tier: string(r.Tier), TenantID: r.TenantID, RepoID: r.RepoID, Nature: string(r.Nature),
			CostUSD: r.CostUSD, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			AggregateTokens: r.AggregateTokens,
			Runs:            r.Runs, Backends: r.Backends,
		})
		if r.Nature == credusage.NatureMetered {
			out.MeteredUSD += r.CostUSD
		} else {
			out.EstimatedUSD += r.CostUSD
		}
	}
	return out
}

// tierOrPlatform reads the admin route's optional `?tier=`. An ABSENT
// filter defaults to the platform tier, the one no tenant view can show.
//
// A value it does not know is an ERROR, not a default: silently answering
// with the platform tier's numbers for `?tier=org` is a wrong answer, not a
// missing feature — the caller asked what one key cost and got another's.
// That is how TierOrg was missed here in the first place, the switch being
// closed and its fallthrough silent.
func tierOrPlatform(raw string) (credusage.Tier, error) {
	if raw == "" {
		return credusage.TierPlatform, nil
	}
	switch t := credusage.Tier(raw); t {
	case credusage.TierTeam, credusage.TierOrg, credusage.TierPool, credusage.TierPlatform:
		return t, nil
	default:
		return "", fmt.Errorf("unknown tier %q (want team|org|pool|platform)", raw)
	}
}

// monthFromQuery reads the optional `?month=YYYY-MM` these listings key on,
// defaulting to the current month.
//
// A value it cannot parse is an ERROR, for the reason an unknown `?tier=`
// is: the response carries a `month` field, so serving the current month to
// a caller who asked for another is a wrong answer wearing the right label.
// Measured — a probe asking for `2026-08` was served September, byte for
// byte, and the identical numbers read as a counter that had stopped
// (#1087).
func monthFromQuery(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now, nil
	}
	when, err := time.Parse("2006-01", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("unknown month %q (want YYYY-MM)", raw)
	}
	return when.UTC(), nil
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
	when, merr := monthFromQuery(r.URL.Query().Get("month"), time.Now().UTC())
	if merr != nil {
		httpError(w, http.StatusBadRequest, "%s", merr.Error())
		return
	}
	scope := credentialUsageScope{TeamID: teamID}
	var (
		rows []credusage.MonthlyUsage
		err  error
	)
	if repo := strings.TrimSpace(r.URL.Query().Get("repo")); repo != "" {
		scope.Repo = repo
		// ListByRepo spans TENANTS by design — a repository can be served by
		// several teams' credentials — so this route, which answers for one
		// team, must narrow it. Filtering here rather than asking the counter
		// for a tenant-scoped variant keeps the store surface small; the cost
		// is that forgetting this line leaks another team's spend, which is
		// why oneTenant is named and tested rather than inlined.
		var all []credusage.MonthlyUsage
		all, err = s.credUsage.ListByRepo(r.Context(), when, repo)
		rows = oneTenant(all, teamID)
	} else {
		rows, err = s.credUsage.List(r.Context(), when, teamID)
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toCredentialUsageList(when.Format("2006-01"), scope, rows))
}

// oneTenant keeps only the rows belonging to a tenant. The cross-tenant
// listings are super-admin answers; a team route that forwarded one would
// report another team's spend under this team's heading.
func oneTenant(rows []credusage.MonthlyUsage, tenantID string) []credusage.MonthlyUsage {
	out := make([]credusage.MonthlyUsage, 0, len(rows))
	for _, row := range rows {
		if row.TenantID == tenantID {
			out = append(out, row)
		}
	}
	return out
}

// handleAdminCredentialUsage is the platform view: one credential across
// every tenant it served (`?fingerprint=`), one repository across every
// credential that served IT (`?repo=`), or the platform tier's own month.
// Super-admin, because both cross-tenant answers name the tenants.
func (s *Server) handleAdminCredentialUsage(w http.ResponseWriter, r *http.Request) {
	if s.credUsage == nil {
		httpError(w, http.StatusNotFound, "per-credential usage is not enabled on this instance")
		return
	}
	when, merr := monthFromQuery(r.URL.Query().Get("month"), time.Now().UTC())
	if merr != nil {
		httpError(w, http.StatusBadRequest, "%s", merr.Error())
		return
	}
	var (
		rows  []credusage.MonthlyUsage
		err   error
		scope credentialUsageScope
	)
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	switch {
	case fp != "" && repo != "":
		// Refused rather than served by whichever the code checks first: the
		// caller asked a question this endpoint does not answer, and picking
		// one of the two silently returns a number that is not what was
		// asked for — the same failure `?tier=org` used to have here.
		httpError(w, http.StatusBadRequest, "give ?fingerprint= or ?repo=, not both — one credential across repositories and one repository across credentials are different questions")
		return
	case repo != "":
		scope.Repo = repo
		rows, err = s.credUsage.ListByRepo(r.Context(), when, repo)
	case fp != "":
		scope.Fingerprint = fp
		rows, err = s.credUsage.ListByFingerprint(r.Context(), when, fp)
	default:
		// By TIER, not by tenant: a platform credential is metered under
		// each tenant it served, so no single tenant holds its month.
		tier, terr := tierOrPlatform(r.URL.Query().Get("tier"))
		if terr != nil {
			httpError(w, http.StatusBadRequest, "%s", terr.Error())
			return
		}
		// Echoed even — especially — when the caller named no tier: this
		// listing is one tier's, and the default is the one a reader is
		// least likely to have in mind.
		scope.Tier = string(tier)
		rows, err = s.credUsage.ListByTier(r.Context(), when, tier)
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toCredentialUsageList(when.Format("2006-01"), scope, rows))
}
