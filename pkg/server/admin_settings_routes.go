package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// Platform runtime settings — the DB-backed form of the operational env
// vars, per the doctrine the platform LLM credentials established: env =
// default, DB record = runtime override, effective without restart. First
// family: the usage-cap percentages (ITERION_USAGE_CAP_5H_PCT /
// ITERION_USAGE_CAP_WEEK_PCT).
//
// Super-admin only: the caps protect the deployment's own subscription,
// so retuning them is a platform decision — same guard as the platform
// credential routes.
func (s *Server) registerAdminSettingsRoutes() {
	if s.authSvc == nil || s.usageCapSettings == nil {
		return
	}
	s.mux.Handle("GET /api/admin/settings/usage-caps", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetUsageCaps)))
	s.mux.Handle("PUT /api/admin/settings/usage-caps", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutUsageCaps)))
}

// usageCapsView is the read shape both routes answer with: the record,
// the env defaults, and the EFFECTIVE resolution — so an operator sees in
// one response what will be enforced and where each number came from.
type usageCapsView struct {
	// Record is the stored override record; null when none exists (env
	// defaults apply everywhere).
	Record *usagecap.Settings `json:"record"`
	// Env are the env-resolved default percentages.
	Env struct {
		FiveHourPct float64 `json:"five_hour_pct"`
		WeekPct     float64 `json:"week_pct"`
	} `json:"env"`
	// Effective is the enforced policy (db-or-env) as the enforcement
	// points resolve it.
	Effective struct {
		FiveHourPct  float64 `json:"five_hour_pct"`
		FiveHourMode string  `json:"five_hour_mode"`
		WeekPct      float64 `json:"week_pct"`
		WeekMode     string  `json:"week_mode"`
	} `json:"effective"`
	// Source is "env", "db" or "db+env" — which tier the effective
	// percentages come from.
	Source string `json:"source"`
	// PropagationBoundSeconds is the worst-case delay before every
	// replica of both deployments enforces an update (the resolver TTL).
	PropagationBoundSeconds int `json:"propagation_bound_seconds"`
}

func (s *Server) usageCapsViewNow(r *http.Request) (usageCapsView, error) {
	var view usageCapsView
	envPol, err := usagecap.FromEnv()
	if err != nil {
		return view, fmt.Errorf("env usage-cap policy is invalid: %w", err)
	}
	rec, err := s.usageCapSettings.GetSettings(r.Context())
	if err != nil {
		return view, fmt.Errorf("read settings: %w", err)
	}
	view.Record = rec
	view.Env.FiveHourPct = envPol.FiveHour.MaxPercent
	view.Env.WeekPct = envPol.Week.MaxPercent
	eff := rec.Apply(envPol)
	view.Effective.FiveHourPct = eff.FiveHour.MaxPercent
	view.Effective.FiveHourMode = string(eff.FiveHour.Mode)
	view.Effective.WeekPct = eff.Week.MaxPercent
	view.Effective.WeekMode = string(eff.Week.Mode)
	view.Source = usagecap.Origin{
		FiveHourDB: rec != nil && rec.FiveHourPct != nil,
		WeekDB:     rec != nil && rec.WeekPct != nil,
	}.String()
	view.PropagationBoundSeconds = int(usagecap.DefaultSettingsTTL / time.Second)
	return view, nil
}

func (s *Server) handleAdminGetUsageCaps(w http.ResponseWriter, r *http.Request) {
	view, err := s.usageCapsViewNow(r)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, view)
}

// handleAdminPutUsageCaps updates the override record with merge
// semantics: a field present with a number sets that override, present
// with null clears it (back to env), absent leaves it untouched. Merge —
// not full replace — so a CLI call that names one window can never
// silently clear the other.
func (s *Server) handleAdminPutUsageCaps(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body: %s", err.Error())
		return
	}

	// Loud on anything not understood: a typo'd field name that was
	// silently ignored would read as "the cap is set" while nothing
	// changed — the exact divergence this API exists to end.
	for k := range body {
		if k != "five_hour_pct" && k != "week_pct" {
			httpError(w, http.StatusBadRequest,
				"unknown field %q (want five_hour_pct and/or week_pct)", k)
			return
		}
	}
	if len(body) == 0 {
		httpError(w, http.StatusBadRequest, "empty update: set five_hour_pct and/or week_pct (a number 0–100, or null to clear the override)")
		return
	}

	// parse returns (nil, false, nil) when the field is absent,
	// (nil, true, nil) for an explicit null (clear the override).
	parse := func(field string) (*int, bool, error) {
		raw, ok := body[field]
		if !ok {
			return nil, false, nil
		}
		if string(raw) == "null" {
			return nil, true, nil
		}
		v, err := strconv.Atoi(string(raw))
		if err != nil {
			return nil, false, fmt.Errorf("%s: %s is not an integer (want 0–100, or null to clear)", field, raw)
		}
		return &v, true, nil
	}

	five, fiveSet, err := parse("five_hour_pct")
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	week, weekSet, err := parse("week_pct")
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	prev, err := s.usageCapSettings.GetSettings(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "read settings: %s", err.Error())
		return
	}
	next := usagecap.Settings{}
	if prev != nil {
		next.FiveHourPct, next.WeekPct = prev.FiveHourPct, prev.WeekPct
	}
	if fiveSet {
		next.FiveHourPct = five
	}
	if weekSet {
		next.WeekPct = week
	}
	if err := next.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	id, _ := auth.FromContext(r.Context())
	next.UpdatedAt = time.Now().UTC()
	next.UpdatedBy = id.UserID
	if err := s.usageCapSettings.PutSettings(r.Context(), next); err != nil {
		httpError(w, http.StatusInternalServerError, "write settings: %s", err.Error())
		return
	}
	// The pod that served the update is coherent immediately; every other
	// replica converges within the resolver TTL.
	if s.usageCapSource != nil {
		s.usageCapSource.Invalidate()
	}

	// Audit names old value, new value and the caller (actor derived from
	// the request context inside auditWrite).
	fmtPct := func(v *int) any {
		if v == nil {
			return nil
		}
		return *v
	}
	s.auditPlatform(r, "", "platform.settings.usage_caps.updated", "platform_settings", "usage_caps", map[string]any{
		"old_five_hour_pct": fmtPct(settingsField(prev, func(p *usagecap.Settings) *int { return p.FiveHourPct })),
		"new_five_hour_pct": fmtPct(next.FiveHourPct),
		"old_week_pct":      fmtPct(settingsField(prev, func(p *usagecap.Settings) *int { return p.WeekPct })),
		"new_week_pct":      fmtPct(next.WeekPct),
	})

	view, err := s.usageCapsViewNow(r)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, view)
}

// settingsField reads one field of a possibly-nil record.
func settingsField(rec *usagecap.Settings, get func(*usagecap.Settings) *int) *int {
	if rec == nil {
		return nil
	}
	return get(rec)
}
