package server

import (
	"net/http"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/sessionboard"
)

// serverInfoResponse describes the running server to the SPA. Used by
// the Launch modal to render appropriate upload limits before any
// upload is attempted, and by the AuthProvider to decide whether to
// gate the studio on a sign-in flow.
type serverInfoResponse struct {
	Mode    string `json:"mode"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	// AuthRequired is false in local / desktop mode (single-user TTY,
	// no JWT) and true in cloud mode (multitenant). The SPA short-
	// circuits its bootstrap when false and renders the studio as a
	// synthetic super-admin so the existing local UX is preserved.
	AuthRequired bool              `json:"auth_required"`
	Limits       serverLimitsBlock `json:"limits"`
	// WorkDir is the absolute working directory the server was launched
	// with (`iterion studio --dir`). Empty in cloud mode where there is
	// no per-server folder concept.
	WorkDir string `json:"work_dir,omitempty"`
	// ProjectName is a human-friendly label derived from WorkDir
	// (typically its basename). The SPA surfaces it in the Toolbar and
	// RunHeader so the user always sees which project they're editing.
	ProjectName string `json:"project_name,omitempty"`
	// CurrentProjectID matches the registry entry currently selected
	// (when the SPA wants to highlight it in the ProjectSwitcher).
	// Empty in cloud mode or when the registry has never been written.
	CurrentProjectID string `json:"current_project_id,omitempty"`
	// BrowseRoot is the absolute path under which the server-side
	// directory browser (/api/filesystem/list) is allowed to traverse,
	// or "" when the feature is disabled. The SPA shows the Browse
	// button in the AddProject dialog only when this is non-empty.
	BrowseRoot string `json:"browse_root,omitempty"`
	// NativeTrackerEnabled is true when the server has the native
	// kanban store wired. The SPA conditionally exposes the Board view.
	NativeTrackerEnabled bool `json:"native_tracker_enabled"`
	// DispatcherEnabled is true when a Dispatcher instance is running on
	// the server. The SPA conditionally exposes the Dispatcher view.
	DispatcherEnabled bool `json:"dispatcher_enabled"`
	// CostCapEnabled is true when a per-(store, UTC-day) LLM spend cap is
	// configured. The SPA polls GET /api/v1/limits/cost for live status
	// and renders the cost-cap banner only when this is true.
	CostCapEnabled bool `json:"cost_cap_enabled"`
	// EmailEnabled is true when a real SMTP mailer is wired. The SPA
	// only offers email-dependent flows (forgot-password, "send
	// invitation by email") when true.
	EmailEnabled bool `json:"email_enabled"`
	// MarketplaceEnabled is true when the hosted bot registry store is
	// wired (Config.Marketplace). The SPA conditionally exposes the
	// Marketplace view + nav entry.
	MarketplaceEnabled bool `json:"marketplace_enabled"`
	// TriggersEnabled is true when the event-driven trigger store is wired
	// (Config.TriggerStore). The SPA conditionally exposes the Triggers /
	// Automations view that manages board (and future) subscriptions.
	TriggersEnabled bool `json:"triggers_enabled"`
	// ConfigSharesEnabled is true when the scoped config-share editor is wired
	// (store + generic-secret stack + auth). The SPA exposes the operator
	// "Share config" surface when set.
	ConfigSharesEnabled bool `json:"config_shares_enabled"`
	// ForgeGitHubAppConfigured is true when this server has a GitHub App
	// configured (ITERION_FORGE_GITHUB_APP_*). The forge connect form only
	// offers the "Install GitHub App" mode when true — otherwise selecting it
	// dead-ends on a 400 ("the GitHub App is not configured on this server").
	// OAuth / PAT connect modes don't depend on it.
	ForgeGitHubAppConfigured bool `json:"forge_github_app_configured"`
	// SessionBoardEnabled is true when the LLM Session-board curation layer
	// is on for this server (ITERION_SESSION_BOARD). The run console's Tasks
	// tab only fetches the curated widget spec when true — when off it
	// renders the deterministic task-list board alone and never polls.
	SessionBoardEnabled bool `json:"session_board_enabled"`
	// PluginsEnabled is always true: the plugin registry (embedded builtins +
	// ~/.iterion/plugins) is available in every mode, so the SPA can surface a
	// Plugins management view unconditionally.
	PluginsEnabled bool `json:"plugins_enabled"`
	// WebPushEnabled is true when the server can deliver browser push
	// notifications (subscription store + VAPID keypair wired). The SPA
	// surfaces the Notifications settings panel only when true.
	WebPushEnabled bool `json:"web_push_enabled"`
	// WebPushVAPIDPublicKey is the applicationServerKey browsers subscribe
	// with. Public by design (it is embedded in every push registration).
	WebPushVAPIDPublicKey string `json:"web_push_vapid_public_key,omitempty"`
	// SecretsEnabled is true in local (non-cloud) mode when a sealed secret
	// store + sealer are wired, so the SPA can surface the local Secrets
	// management view. Cloud mode uses the auth-gated team/personal secrets
	// UI instead (never this flag).
	SecretsEnabled bool `json:"secrets_enabled"`
	// SkillsEnabled is true in local (non-cloud) mode: the SPA surfaces the
	// Skills library management view (/api/local/skills). No sealing is
	// involved, so unlike SecretsEnabled it gates on mode alone.
	SkillsEnabled bool `json:"skills_enabled"`
	// RunShellEnabled is true in local (non-cloud) mode: the SPA offers
	// the post-mortem shell (GET /api/ws/runs/{id}/shell) on terminal
	// runs whose preserved worktree still exists. Never in cloud —
	// spawning an interactive host shell from a multi-tenant API is not
	// a thing.
	RunShellEnabled bool `json:"run_shell_enabled"`
	// PipelineConcurrency reports the local pipeline-concurrency gate
	// (max/active/waiting) so the pipeline board can render the cap + how
	// many pipelines wait for a slot. Enabled=false when no cap is set.
	PipelineConcurrency runview.PipelineConcurrencyStatus `json:"pipeline_concurrency"`
}

type serverLimitsBlock struct {
	Upload uploadLimitsBlock `json:"upload"`
}

type uploadLimitsBlock struct {
	MaxFileSize    int64    `json:"max_file_size"`
	MaxTotalSize   int64    `json:"max_total_size"`
	MaxFilesPerRun int      `json:"max_files_per_run"`
	AllowedMIME    []string `json:"allowed_mime"`
}

// handleServerInfo answers GET /api/server/info. Public (no
// origin gate) because it returns inert metadata used by the SPA.
func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	mode := s.cfg.Mode
	if mode == "" {
		mode = "local"
	}
	resp := serverInfoResponse{
		Mode:         mode,
		Version:      appinfo.Version,
		Commit:       appinfo.Commit,
		AuthRequired: s.authSvc != nil && !s.cfg.DisableAuth,
		Limits: serverLimitsBlock{
			Upload: uploadLimitsBlock{
				MaxFileSize:    s.cfg.MaxUploadSize,
				MaxTotalSize:   s.cfg.MaxTotalUploadSize,
				MaxFilesPerRun: s.cfg.MaxUploadsPerRun,
				AllowedMIME:    s.cfg.AllowedUploadMIMEs,
			},
		},
		// True for the self-hosted filesystem board AND the cloud per-team
		// Mongo board — both expose the same /api/v1/native surface, so the
		// studio Board view is gated identically.
		NativeTrackerEnabled:     s.cfg.NativeTrackerStore != nil || s.cfg.CloudBoardFor != nil,
		DispatcherEnabled:        s.cfg.Dispatcher != nil,
		EmailEnabled:             s.authSvc != nil && s.authSvc.EmailEnabled(),
		MarketplaceEnabled:       s.marketplace != nil,
		TriggersEnabled:          s.cfg.TriggerStore != nil,
		ConfigSharesEnabled:      s.configShares != nil && s.genericSecrets != nil && s.sealer != nil && s.authSvc != nil,
		ForgeGitHubAppConfigured: s.forgeGitHubApp.Configured(),
		SessionBoardEnabled:      sessionboard.Enabled(),
		PluginsEnabled:           true,
		WebPushEnabled:           s.webPushEnabled(),
	}
	if resp.WebPushEnabled {
		resp.WebPushVAPIDPublicKey = s.cfg.WebPushVAPIDPublicKey
	}
	// Snapshot the hot-swap fields under a single RLock (both read here are
	// swapped on a project switch): the run-console service for the daily-cap
	// probe, and the local secret store for the Secrets-view gate.
	s.stateMu.RLock()
	runsSvc := s.runs
	localSecrets := s.localSecrets
	s.stateMu.RUnlock()
	resp.SecretsEnabled = s.cfg.Mode != "cloud" && localSecrets != nil && s.sealer != nil
	resp.SkillsEnabled = s.cfg.Mode != "cloud"
	resp.RunShellEnabled = s.cfg.Mode != "cloud"
	// Surface whether the daily spend cap is active so the SPA knows to
	// poll for live status. DailyCap() is nil when disabled.
	if runsSvc != nil && runsSvc.DailyCap() != nil {
		resp.CostCapEnabled = true
	}
	if runsSvc != nil {
		resp.PipelineConcurrency = runsSvc.PipelineConcurrency()
	}
	if mode == "local" {
		resp.WorkDir = s.cfg.WorkDir
		resp.ProjectName = deriveProjectName(s.cfg.WorkDir)
		resp.BrowseRoot = browseRoot()
		resp.CurrentProjectID = s.CurrentProjectID()
	}
	s.writeJSONFor(w, r, resp)
}

// deriveProjectName picks a human-friendly label from the working
// directory. Returns "" for empty / root-ish inputs so the SPA can fall
// back to no-label rendering.
func deriveProjectName(dir string) string {
	if dir == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == "/" || base == string(filepath.Separator) {
		return ""
	}
	return base
}
