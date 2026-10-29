package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
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
	// The run-level permission knob rides to the preview's screen, so it
	// answers the launch the caller is about to shape — whose request
	// carries the same field. Unwired, an "off" escape hatch would refuse
	// a legal launch and an intended "deny" would bless one the launch
	// refuses.
	spec.Launch.Permission = strings.TrimSpace(req.Permission)
	disabled := false
	previewRepo := ""
	switch req.Source.Kind {
	case "personal":
		if req.Source.ID != "" {
			httpError(w, http.StatusBadRequest, "personal source does not accept an id")
			return
		}
		// The caller's vars ride as the personal lane's own --var-style
		// values: the operator owns them (ADR-129 §4 rule 2 trusts the
		// CLI's --var names), so the preview reads the routing fields the
		// way the launch it mirrors would. The webhook kind resolves its
		// vars from the config layers and the delivery payload — caller
		// values are not part of that launch shape and are refused, never
		// silently dropped.
		spec.Launch.Vars = req.Vars
		spec.Launch.TrustedVarNames = trustedVarNames(req.Vars)
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
		spec.OwnerID = webhookActor(cfg.ID)
		spec.Launch.KeyOverrides = cfg.KeyOverrides
		// The repository the webhook serves rides the preview's context as
		// it rides the launch's: a key restricted to repositories previews
		// as withheld for any other, and as withheld when the webhook
		// serves several (the launch would name one; the preview cannot).
		// Only a LITERAL entry is a repository: the allowlist is a pattern
		// list (webhooks.MatchProject — "*", "group/*"), and a pattern
		// stamped as the repository would preview a key restricted to
		// "group/api" as withheld while every delivery spends it.
		// Trimmed as webhooks.MatchProject reads it, so the preview and the
		// delivery agree on the entry.
		if len(cfg.ProjectAllowlist) == 1 {
			if entry := strings.TrimSpace(cfg.ProjectAllowlist[0]); !strings.Contains(entry, "*") {
				previewRepo = entry
			}
		}
		if len(req.Vars) > 0 {
			httpError(w, http.StatusBadRequest, "webhook source resolves its vars from the config layers and the delivery payload; a caller-supplied vars map is not part of that launch")
			return
		}
		spec.Launch.Vars = applyWebhookVarLayers(map[string]string{}, cfg)
		// The launch trusts the config's OPERATOR layer for the routing
		// pre-flights (webhooks_gitlab.go's webhook lane); the preview
		// answers for the same launch, so it trusts the same names — a
		// var-derived cell the launch narrows must not preview as undecided.
		spec.Launch.TrustedVarNames = trustedVarNames(cfg.OperatorLaunchVars)
		// The webhook lanes launch warn-and-proceed on an undeclared input
		// (AllowUnknownInputs); the preview mirrors its lane, the personal
		// one mirrors the CLI's default refusal.
		spec.Launch.AllowUnknownInputs = true
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
	ctx := secrets.WithLaunchRepo(store.WithTenant(r.Context(), teamID), previewRepo)
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
	// The preview's whitelist must answer what the LAUNCH will seal: the
	// policy resolved exactly as the launch site resolves it — the run
	// level the caller sent folded as the HEAD Launcher layer, the bot's
	// manifest layer and the team beneath (a ("","") resolution would
	// drop them and preview a policy the launch never folds).
	var runLayers []llmroute.Layer
	if req.LLMRouting != nil {
		runLayers = append(runLayers, llmroute.Layer{Source: llmroute.SourceRun, Launcher: true, Policy: *req.LLMRouting})
	}
	routePolicy, routeErr := s.resolveRunLLMRoutePolicy(ctx, spec.Context.TeamID, spec.Context.BotID, runLayers...)
	if routeErr != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", routeErr)
		return
	}
	spec.LLMRoutePolicy = routePolicy
	out, err := s.runs.PreviewCredentials(ctx, spec)
	if errors.Is(err, runview.ErrCredentialPreviewUnavailable) {
		httpError(w, http.StatusServiceUnavailable, "credential preview is unavailable")
		return
	}
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "credential preview could not be evaluated: %v", err)
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
