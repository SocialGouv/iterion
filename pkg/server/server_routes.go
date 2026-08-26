package server

import (
	"io/fs"
	"net/http"
)

func (s *Server) routes() {
	// CORS preflight handler — only echoes ACAO when the Origin is an
	// allowed loopback origin. The wildcard ACAO previously emitted here
	// (combined with POST /api/files/save accepting JSON bodies) allowed
	// any browser tab the user visited to write attacker-controlled workflow
	// files into WorkDir, which iterion would then execute under `sh -c`
	// the next time the user ran the workflow — drive-by RCE on the dev
	// machine. The 'local-only server' framing didn't address this because
	// the threat is browser-side, not network-side.
	s.mux.HandleFunc("OPTIONS /api/", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !s.isAllowedOriginReq(r) {
			// No ACAO header → browser blocks the cross-origin request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})

	s.mux.HandleFunc("POST /api/parse", s.handleParse)
	s.mux.HandleFunc("POST /api/unparse", s.handleUnparse)
	s.mux.HandleFunc("POST /api/validate", s.handleValidate)
	s.mux.HandleFunc("GET /api/examples", s.handleListExamples)
	s.mux.HandleFunc("GET /api/examples/{name...}", s.handleLoadExample)
	s.mux.HandleFunc("GET /api/effort-capabilities", s.handleEffortCapabilities)
	s.mux.HandleFunc("GET /api/resolve-effort", s.handleResolveEffort)
	s.mux.HandleFunc("GET /api/resolve-model", s.handleResolveModel)
	s.mux.HandleFunc("GET /api/backends/detect", s.handleBackendsDetect)
	// The model registry: known x usable x capabilities x pricing. It is what
	// turns the studio's free-text model field into an actual picker.
	s.mux.HandleFunc("GET /api/models", s.handleModels)

	// Bot registry — exposes the bots discoverable on the host (single
	// .bot files, .botz bundles) with their declared workflow vars +
	// presets so the studio's Board ticket form can render a typed
	// args form per bot. Read-only, gated by the same auth middleware
	// as the rest of /api/* (the wrapper at line ~427 wraps the mux).
	s.mux.HandleFunc("GET /api/v1/plugins", s.handlePluginsList)
	s.mux.HandleFunc("GET /api/v1/plugins/{name}", s.handlePluginDetail)
	// enable/disable rewrite the host-global plugins.yaml — on a shared
	// cloud server that changes behavior for every tenant, so they take
	// the same super-admin gate as install (operator-open in local mode).
	s.mux.Handle("POST /api/v1/plugins/{name}/enable", s.requireSuperAdmin(s.handlePluginEnable(true)))
	s.mux.Handle("POST /api/v1/plugins/{name}/disable", s.requireSuperAdmin(s.handlePluginEnable(false)))
	// install/uninstall mutate the shared plugin tree (clone an arbitrary
	// source server-side), so they're gated to platform super-admins —
	// requireSuperAdmin synthesizes a super-admin in local/dev mode, so the
	// single-user operator keeps full access.
	s.mux.Handle("POST /api/v1/plugins/install", s.requireSuperAdmin(http.HandlerFunc(s.handlePluginInstall)))
	s.mux.Handle("DELETE /api/v1/plugins/{name}", s.requireSuperAdmin(http.HandlerFunc(s.handlePluginUninstall)))
	// config write can carry secrets and is instance-global, so it's super-admin
	// gated too (operator-open in local/dev mode).
	s.mux.Handle("PUT /api/v1/plugins/{name}/config", s.requireSuperAdmin(http.HandlerFunc(s.handlePluginConfig)))
	// lifecycle executes manifest shell in the workspace, so it takes the same
	// gate as install (super-admin; handler adds safe-origin + local-mode-only).
	s.mux.Handle("POST /api/v1/plugins/{name}/lifecycle/{phase}", s.requireSuperAdmin(http.HandlerFunc(s.handlePluginLifecycle)))
	s.mux.HandleFunc("GET /api/v1/bots", s.handleBotsList)
	// Bot creation (studio builder) — scaffolds a bundle into the
	// workspace bots/ dir; the literal /templates route wins over {name}.
	s.mux.HandleFunc("POST /api/v1/bots", s.handleBotCreate)
	s.mux.HandleFunc("GET /api/v1/bots/templates", s.handleBotTemplates)
	s.mux.HandleFunc("POST /api/v1/bots/install", s.handleBotInstall)
	s.mux.HandleFunc("POST /api/v1/bots/upload", s.handleBotUpload)
	s.mux.HandleFunc("POST /api/v1/bots/import", s.handleBotImport)
	s.mux.HandleFunc("GET /api/v1/bots/{name}", s.handleBotsGet)
	s.mux.HandleFunc("PUT /api/v1/bots/{name}", s.handleBotsPut)
	s.mux.HandleFunc("PUT /api/v1/bots/{name}/overlay", s.handleBotOverlay)
	// Pipeline Boards are a bot-bound execution projection layered alongside
	// the native backlog board. They keep their own routes/read model so the
	// existing /board tracker and dispatcher semantics remain unchanged.
	s.registerPipelineBoardRoutes()

	// Hosted marketplace — curated registry of bot bundles published by
	// repos. Each endpoint short-circuits to 404 when s.marketplace is
	// nil so wiring is single-config (the studio gates its view on
	// MarketplaceEnabled in /api/server/info).
	s.mux.HandleFunc("GET /api/v1/marketplace/bots", s.handleMarketplaceList)
	s.mux.HandleFunc("POST /api/v1/marketplace/submit", s.handleMarketplaceSubmit)
	s.mux.HandleFunc("GET /api/v1/marketplace/config", s.handleMarketplaceConfig)
	s.mux.HandleFunc("GET /api/v1/marketplace/bots/{slug}", s.handleMarketplaceGet)
	// Public .botz download (see isPublicMarketplaceRead) — packs the
	// bundle on demand from its source coordinates.
	s.mux.HandleFunc("GET /api/v1/marketplace/bots/{slug}/download", s.handleMarketplaceDownload)
	s.mux.HandleFunc("POST /api/v1/marketplace/bots/{slug}/install", s.handleMarketplaceInstall)
	s.mux.HandleFunc("DELETE /api/v1/marketplace/bots/{slug}/install", s.handleMarketplaceUninstall)
	// Moderation (cloud-only; handlers 404 in local mode).
	s.mux.HandleFunc("GET /api/v1/marketplace/moderation", s.handleMarketplaceModerationList)
	s.mux.HandleFunc("POST /api/v1/marketplace/moderation/{slug}/approve", s.handleMarketplaceApprove)
	s.mux.HandleFunc("POST /api/v1/marketplace/moderation/{slug}/reject", s.handleMarketplaceReject)

	// Health endpoints — liveness (always 200 if the mux is alive)
	// and readiness (cloud-mode dependency pings come via T-26 when
	// Mongo/NATS/S3 ping handles are threaded into the server). Used
	// by the Helm chart probes (plan §F T-36, T-37).
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// WebSocket endpoint for file watching
	s.mux.HandleFunc("GET /api/ws", s.hub.HandleWebSocket)

	// File management endpoints
	s.mux.HandleFunc("GET /api/files", s.handleListFiles)
	s.mux.HandleFunc("POST /api/files/open", s.handleOpenFile)
	s.mux.HandleFunc("POST /api/files/save", s.handleSaveFile)

	// Project registry — lets the SPA list MRU projects, switch
	// between them, and add/remove entries. The same on-disk file
	// (<UserConfigDir>/Iterion/config.json) is shared with the desktop
	// (Wails) app via pkg/server/projects.
	s.mux.HandleFunc("GET /api/projects", s.handleListProjects)
	s.mux.HandleFunc("GET /api/projects/current", s.handleCurrentProject)
	s.mux.HandleFunc("POST /api/projects", s.handleAddProject)
	s.mux.HandleFunc("POST /api/projects/switch", s.handleSwitchProject)
	s.mux.HandleFunc("DELETE /api/projects/{id}", s.handleRemoveProject)
	// Server-side directory browser, gated by the ITERION_BROWSE_ROOT
	// env var. Used by the web-mode AddProject dialog so the user can
	// pick a folder without typing its absolute path. Disabled (403)
	// when the env var is unset, so a default deployment never exposes
	// the filesystem.
	s.mux.HandleFunc("GET /api/filesystem/list", s.handleFilesystemList)

	// Run console endpoints (registered only when s.runs is wired).
	s.registerRunRoutes()
	s.registerRunLogRoutes()

	// Auth + identity endpoints (login, logout, refresh, OIDC,
	// teams, invitations). Registered only when authSvc is wired —
	// local mode without an auth service skips them.
	if s.authSvc != nil {
		s.registerAuthRoutes()
	}

	// BYOK endpoints. Requires the auth+identity stack already in
	// place — caller must wire AuthService + ApiKeys + Sealer.
	if s.apiKeys != nil && s.sealer != nil && s.authSvc != nil {
		s.registerBYOKRoutes()
	}
	if s.genericSecrets != nil && s.sealer != nil && s.authSvc != nil {
		s.registerGenericSecretRoutes()
	}
	// Local (non-cloud) single-operator secret store: unauthenticated
	// /api/local/secrets, gated on local mode + a wired store + sealer.
	if s.cfg.Mode != "cloud" && s.localSecrets != nil && s.sealer != nil {
		s.registerLocalSecretRoutes()
	}

	// Local (non-cloud) single-operator skill library: unauthenticated
	// /api/local/skills, gated on local mode (no sealing needed — a skill is
	// public guidance text, not a secret). The store is built on demand from
	// the current store dir, so no wired backend is required here.
	if s.cfg.Mode != "cloud" {
		s.registerLocalSkillRoutes()
	}

	// Inbound webhook spine: per-org webhook token CRUD. The inbound
	// /api/webhooks/{provider}/{id} delivery routes are registered by
	// each provider (see registerGitLabWebhookRoute).
	if s.webhookConfigs != nil && s.authSvc != nil {
		s.registerWebhookRoutes()
	}
	// Inbound delivery routes (self-authenticating via webhookAuth) —
	// one per supported provider, registered whenever the config store
	// is present. Each handler is independent: a tenant who only wires
	// up GitLab can ignore the GitHub/Forgejo/generic URLs.
	if s.webhookConfigs != nil {
		s.registerGitLabWebhookRoute()
		s.registerGitHubWebhookRoute()
		s.registerForgejoWebhookRoute()
		s.registerGenericWebhookRoute()
	}

	// Bot-secret bindings (policy wrapper over generic secrets).
	if s.botBindings != nil && s.authSvc != nil {
		s.registerBotBindingRoutes()
		s.registerPluginSourceRoutes()
	}

	// Team-authored bot sources — the writable, tenant-scoped bot store that
	// makes cloud bot editing possible. Needs the store + the auth stack
	// (team-scoped CRUD behind canEditBots).
	if s.botSources != nil && s.authSvc != nil {
		s.registerBotSourceRoutes()
	}

	// Recurring cloud schedules — team-scoped CRUD. Cloud-only (the ticker
	// that fires them lives on the same Server.cfg.ScheduledBots handle).
	if s.cfg.ScheduledBots != nil && s.authSvc != nil {
		s.registerScheduleRoutes()
	}

	// Scoped config-share editor. Needs the store (defaulted in New) + the
	// generic-secret stack (to resolve the repo's forge_token) + the auth
	// stack (operator CRUD). Public self-auth routes + operator CRUD.
	if s.configShares != nil && s.genericSecrets != nil && s.sealer != nil && s.authSvc != nil {
		s.registerConfigSharePublicRoutes()
		s.registerConfigShareAdminRoutes()
		// Authenticated config-editor surface (ADR-078): SSO-team-scoped session
		// editing, distinct from the public token path.
		s.registerConfigEditorRoutes()
	}

	// Outbound forge integrations (connect a repo + auto-provision). Gated
	// on the orchestrator being built (full dependency set) + the auth stack.
	if s.forgeOrchestrator != nil && s.authSvc != nil {
		s.registerForgeRoutes()
		s.registerForgeProvisioningRoutes()
		if s.forgeOAuthApps != nil {
			s.registerForgeOAuthAppRoutes()
		}
		// Cloud board ↔ forge: per-repo issue sync toggle + manual sync, and
		// per-card push-to-forge + linked-PR/CI views (no-op without a cloud
		// board). See board_forge.go.
		s.registerBoardForgeRoutes()
	}

	// Per-tenant SSO providers (a tenant's own Keycloak + GitHub team-gating).
	// Needs the auth stack + a sealer for the OIDC client secret.
	if s.orgSSO != nil && s.authSvc != nil && s.sealer != nil {
		s.registerOrgSSORoutes()
	}
	if s.orgDomains != nil && s.authSvc != nil {
		s.registerOrgSSODomainRoutes()
	}

	// Audit log read surface (writes happen inline in the mutation
	// handlers via auditTenant/auditPlatform). No-op when no store.
	if s.authSvc != nil {
		s.registerAuditRoutes()
	}

	// Personal access tokens (programmatic API access).
	if s.pats != nil && s.authSvc != nil {
		s.registerPATRoutes()
	}

	// Browser push notifications (subscription CRUD + prefs + test push).
	if s.webPushEnabled() && s.authSvc != nil {
		s.registerNotificationRoutes()
	}

	// The operator's remembered model choice for a long-lived surface.
	if s.cfg.ModelPrefs != nil {
		s.registerModelPrefRoutes()
	}

	// Super-admin DLQ inspection/replay (cloud only — needs the queue).
	if s.authSvc != nil {
		s.registerQueueAdminRoutes()
	}

	// Shared-knowledge memory REST (FS fallback when no store wired).
	s.registerMemoryRoutes()

	// OAuth-forfait endpoints. Same gating as BYOK plus the per-
	// user OAuthForfait store.
	if s.oauthStore != nil && s.sealer != nil && s.authSvc != nil {
		s.registerOAuthForfaitRoutes()
		// Team/org-scoped mirror (admin-gated). Only meaningful when the
		// auth store can resolve team membership.
		if s.authStore() != nil {
			s.registerOAuthTeamRoutes()
		}
		// Mutualised credential pool. Rides the same gating: lending a
		// subscription starts by connecting one, so a deployment without
		// the OAuth surface has nothing to pool.
		if s.credPoolPools != nil && s.credPoolPledges != nil && s.credPoolLeases != nil && s.credPoolLedger != nil {
			s.registerCredPoolRoutes()
		}
	}

	// Platform LLM credentials (super-admin): the DB-backed form of the
	// runner-pod env fallback. Gates per credential family internally
	// (api-keys need the BYOK store, oauth the forfait store).
	s.registerAdminLLMRoutes()

	// Platform runtime settings (super-admin): the DB-backed form of the
	// operational env vars — first family: the usage-cap percentages.
	s.registerAdminSettingsRoutes()

	// Dispatcher + native tracker — both optional. Each handler is
	// registered through requireAuth so a server bound to a non-loopback
	// address (devcontainer / LAN / SSH tunnel) can't have its kanban
	// or dispatcher state mutated by an unauthenticated peer. The
	// RegisterRoutesWithMiddleware variants preserve method-specific
	// patterns so they don't conflict with the server's OPTIONS /api/
	// catch-all.
	if s.cfg.NativeTrackerStore == nil && s.cfg.CloudBoardFor != nil {
		// Cloud mode: per-active-team board at the same /api/v1/native prefix
		// the studio board client already uses (see board_cloud_routes.go).
		s.registerCloudBoardRoutes()
	}
	if s.cfg.NativeTrackerStore != nil {
		s.cfg.NativeTrackerStore.RegisterRoutesWithMiddleware(s.mux.ServeMux, "/api/v1/native", s.requireAuth)
		// The Board MCP HTTP endpoint authenticates via its own
		// per-run X-Iterion-Run token (issued by the runtime at
		// run-start), so it intentionally bypasses requireAuth.
		RegisterBoardMCPRoutes(s.mux.ServeMux, "/api/v1/mcp/board", s.cfg.NativeTrackerStore, s.boardMCPTokens)
		// Resolve a "/command" in a board-issue comment into a bot launch (the
		// native/local twin of the forge issue-comment trigger). requireAuth on
		// the comment route already gates who may post; the resolver only adds
		// command→bot routing + the open_mr stamp.
		s.wireNativeBoardCommands()
	}
	if s.cfg.Dispatcher != nil {
		s.cfg.Dispatcher.RegisterRoutesWithMiddleware(s.mux.ServeMux, "/api/v1/dispatcher", s.requireAuth)
	}
	// Deterministic forge review publishing. Like the board MCP endpoint it
	// authenticates via its own per-run X-Iterion-Run token (minted at launch
	// by injectForgePublishVars), so it intentionally bypasses requireAuth.
	if s.forgeConnections != nil && s.forgePublishTokens != nil {
		s.mux.ServeMux.HandleFunc("POST /api/v1/forge/publish-review", s.handleForgePublishReview)
	}
	// Event-driven trigger subscription CRUD backing the Triggers /
	// Automations view. No-op without a TriggerStore.
	if s.cfg.TriggerStore != nil {
		s.registerTriggerRoutes()
	}
	// Cross-run stats aggregation backing /insights. No-op when the
	// server runs without a run-store handle (cloud control plane).
	s.registerRunsStatsRoutes()

	// Distinct repositories (project_path) backing the run-list "by repo"
	// filter chips. No-op without a run-store handle.
	s.registerRunsReposRoutes()

	// Daily spend-cap status + one-click override. No-op without a run
	// store. Gated by the global /api/* auth middleware.
	s.registerLimitsRoutes()

	// Live route inventory as OpenAPI 3 + flat list (GET /api/openapi.json,
	// /api/routes), generated from the recordingMux so it can't drift. Drives
	// `iterion remote openapi` / `routes`. Registered LAST so every route
	// above is captured before the spec is first served.
	s.registerOpenAPIRoutes()

	// Serve static frontend files with SPA fallback so client-side routes
	// (e.g. /runs/abc) render index.html instead of 404.
	staticSub, err := fs.Sub(StaticFS, "static")
	if err != nil {
		// A library must not kill the process: the API surface
		// registered above stays serving, and the failure is reported
		// through the central logger (and, with a DSN configured, the
		// error tracker) instead of a bare stdlib log.Fatalf that skips
		// every defer. Same degradation as `iterion dispatch`.
		s.logger.Error("server: SPA assets unavailable, studio UI not served: %v", err)
		return
	}
	s.mux.Handle("GET /", SPAHandler(staticSub))
}
