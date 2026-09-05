package server

import (
	"net/http"
	"strings"
)

// Usage-window readings — the operator's escape hatch on the shared ledger.
//
// A credential's readings normally expire on their own (their reset
// instant, the trust window). The one case that needs a human is a reset
// the ledger cannot see: the provider reset a window EARLY, the stored
// reading still says 99%, and every run of the credential is refused
// pre-flight — which is precisely what keeps the reading from ever being
// refreshed. Raising the global cap to unstick it lifts the guard for every
// tenant and every bot; this clears ONE credential, by fingerprint, and
// leaves the caps alone.
//
// Super-admin only: the ledger is fleet-wide, and the fingerprint names a
// credential the caller may not otherwise see.
func (s *Server) registerAdminUsageReadingsRoutes() {
	if s.authSvc == nil || s.usageCaps == nil {
		return
	}
	s.mux.Handle("DELETE /api/admin/usage-readings/{fingerprint}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminClearUsageReadings)))
}

// usageReadingsClearedView is the DELETE's answer: which credential was
// forgotten and how many readings that dropped (zero when nothing was
// stored — not an error, the ledger already read "nothing learned yet").
type usageReadingsClearedView struct {
	Fingerprint string `json:"fingerprint"`
	Deleted     int    `json:"deleted"`
}

func (s *Server) handleAdminClearUsageReadings(w http.ResponseWriter, r *http.Request) {
	fp := strings.TrimSpace(r.PathValue("fingerprint"))
	if fp == "" {
		httpError(w, http.StatusBadRequest, "fingerprint is required (the credential's audit fingerprint, as shown on the key/connection views)")
		return
	}
	n, err := s.usageCaps.DeleteByFingerprint(r.Context(), fp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "clear usage readings for %s: %s", fp, err.Error())
		return
	}
	s.auditPlatform(r, "", "platform.usage_readings.cleared", "usage_readings", fp, map[string]any{
		"deleted": n,
	})
	writeJSON(w, usageReadingsClearedView{Fingerprint: fp, Deleted: n})
}
