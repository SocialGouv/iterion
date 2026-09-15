package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func (s *Server) registerCredentialPreviewRoutes() {
	s.mux.Handle("POST /api/teams/{id}/credentials/preview", s.requireAuth(http.HandlerFunc(s.handleCredentialPreview)))
}

// handleCredentialPreview authorizes the target team before resolving any
// source, then derives the exact owner and key pins from that source. Config
// Get is deliberately global, so its tenant must be checked explicitly.
func (s *Server) handleCredentialPreview(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member of this team")
		return
	}
	var req runview.CredentialPreviewRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid credential preview request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		httpError(w, http.StatusBadRequest, "invalid credential preview request")
		return
	}
	req.BotID = strings.TrimSpace(req.BotID)
	spec := runview.CredentialPreviewSpec{Context: runview.CredentialPreviewContext{TeamID: teamID, Source: req.Source}, OwnerID: id.UserID}
	disabled := false
	switch req.Source.Kind {
	case "personal":
		if req.Source.ID != "" {
			httpError(w, http.StatusBadRequest, "personal source does not accept an id")
			return
		}
	case "webhook":
		if req.Source.ID == "" {
			httpError(w, http.StatusBadRequest, "webhook source requires an id")
			return
		}
		if s.webhookConfigs == nil {
			httpError(w, http.StatusNotFound, "webhook not found")
			return
		}
		cfg, err := s.webhookConfigs.Get(r.Context(), req.Source.ID)
		if errors.Is(err, webhooks.ErrNotFound) || err == nil && cfg.TenantID != teamID {
			httpError(w, http.StatusNotFound, "webhook not found")
			return
		}
		if err != nil {
			httpError(w, http.StatusServiceUnavailable, "webhook metadata unavailable")
			return
		}
		if req.BotID == "" {
			req.BotID = cfg.SelectBot()
		}
		if req.BotID == "" {
			httpError(w, http.StatusBadRequest, "bot_id is required for an ambiguous webhook")
			return
		}
		if !cfg.AllowsBot(req.BotID) {
			httpError(w, http.StatusBadRequest, "bot_id is not allowed by this webhook")
			return
		}
		spec.OwnerID = "webhook:" + cfg.ID
		spec.Launch.KeyOverrides = cfg.KeyOverrides
		spec.Launch.Vars = applyWebhookVarLayers(map[string]string{}, cfg)
		disabled = !cfg.Enabled
	default:
		httpError(w, http.StatusBadRequest, "source.kind must be personal or webhook")
		return
	}
	if req.BotID == "" {
		httpError(w, http.StatusBadRequest, "bot_id is required")
		return
	}
	if s.runs == nil {
		httpError(w, http.StatusServiceUnavailable, "credential preview is unavailable")
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	// Raw resolution reads the existing tiered source. Publishing a cloud bot
	// snapshot would be a durable write and has no place in a preview.
	lb, err := s.resolveBotTieredRaw(ctx, teamID, req.BotID, "")
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "bot cannot be resolved for preview")
		return
	}
	if lb == nil {
		httpError(w, http.StatusNotFound, "bot not found")
		return
	}
	defer lb.Cleanup()
	spec.Context.BotID = lb.BotID
	spec.Launch.BotID = lb.BotID
	spec.Launch.FilePath = lb.Path
	spec.Launch.Source = lb.Source
	spec.Launch.BundleDir = lb.BundleDir
	// Catalog source needs its bundle prompts too, using the same path-based
	// promotion as launch compilation. Stored sources carry their materialized
	// bundle directory from the tiered resolver.
	if lb.Origin == "catalog" {
		spec.Launch.Source = ""
	}
	out, err := s.runs.PreviewCredentials(ctx, spec)
	if errors.Is(err, runview.ErrCredentialPreviewUnavailable) {
		httpError(w, http.StatusServiceUnavailable, "credential preview is unavailable")
		return
	}
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "credential preview could not be evaluated")
		return
	}
	if req.Source.Kind == "personal" {
		out.Warnings = append(out.Warnings, "Personal preview uses this bot’s configured model routes, without per-launch Studio or CLI overrides.")
	} else {
		out.Warnings = append(out.Warnings, "Webhook delivery variables are not synthesized. The current credential resolver uses compiled model routes and explicit model overrides, not delivery vars.")
	}
	if disabled {
		out.Warnings = append(out.Warnings, "This webhook is disabled; these credentials would apply if it were enabled.")
	}
	writeJSON(w, out)
}
