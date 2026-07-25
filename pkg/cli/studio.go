package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/store"
)

// StudioOptions holds options for the studio command.
type StudioOptions struct {
	Port      int
	Bind      string // bind address (default "127.0.0.1"); use "0.0.0.0" to expose on LAN
	Dir       string // working directory (for examples)
	StoreDir  string // run store directory (default: nearest .iterion ancestor of Dir, or <Dir>/.iterion)
	NoBrowser bool   // skip opening browser
	// NoBrowserPane disables every Browser-pane code path: the
	// preview proxy, the CDP WS endpoint, and the Chromium runner.
	// Useful for emergency lockdown (security incident) or for
	// shaving startup latency when the operator never needs the
	// pane. Defaults to false (pane enabled).
	NoBrowserPane bool

	// OnReady, when non-nil, is invoked once the HTTP listener is up and
	// the server has accepted its bind address. The argument is the actual
	// host:port the listener is bound to (useful when Port=0 / random).
	// Invoked from a goroutine; callers must be ready for it to fire
	// concurrently with their own Run loop.
	OnReady func(addr string)

	// Mode, when set, advertises the deployment context in
	// /api/server/info and tunes upload defaults ("desktop" raises
	// the per-file cap to 1 GB; "" / "local" / "web" keep the 50 MB
	// cap). The server's Config.Mode owns the same field.
	Mode string

	// MaxUploadSize / MaxTotalUploadSize / MaxUploadsPerRun /
	// AllowUploadMime override the server's upload limits. Zero
	// values fall through to mode-specific defaults applied by
	// pkg/server.applyUploadDefaults.
	MaxUploadSize      int64
	MaxTotalUploadSize int64
	MaxUploadsPerRun   int
	AllowUploadMime    []string

	// MaxConcurrentPipelines caps how many ROOT pipelines run at once
	// (0 = unlimited). Over the cap, launches wait in the pipeline
	// board's Todo lane. Threaded into server.Config →
	// runview.WithMaxConcurrentPipelines.
	MaxConcurrentPipelines int

	// OnForceRefresh, when non-nil, is forwarded to the server and fires
	// before /api/backends/detect?force=1 invalidates its cache. The
	// desktop host uses this to re-source ~/.iterion/env so dotenv
	// edits (including key deletions) are picked up without a restart.
	OnForceRefresh func()

	// BotsPaths configures where the /api/v1/bots endpoint walks to
	// discover bots for the Board ticket form's bot picker. Empty
	// falls back to <Dir>/bots, <Dir>/examples, <Dir>/.botz.
	BotsPaths []string

	// DesktopAlertSink, when non-nil, is wired as the run-health alert
	// desktop delivery sink (the desktop app injects a Wails
	// EventsEmit-backed sink for native OS notifications). It is honoured
	// only when desktop alerts are enabled (ITERION_ALERTS_DESKTOP_ENABLED
	// / alerts.desktop.enabled). The headless CLI and the --server-only
	// daemon leave it nil — they have no Wails runtime to emit through.
	DesktopAlertSink alert.Sink
}

// alertSettingsFromEnv builds the run-health alert settings for local
// studio mode from the ITERION_ALERTS_* environment (the same vars the
// cloud config overlay reads). It always returns a non-nil settings so
// the always-on browser-toast sink + stall detection are default-on;
// the webhook sink activates only when ITERION_ALERTS_WEBHOOK_URL is
// set. The desktop sink stays nil here — Wails delivery is wired by the
// desktop host, not the headless CLI.
//
// BaseURL (used to build clickable /runs/<id> deep links in webhook
// payloads) prefers an explicit ITERION_ALERTS_BASE_URL, else is
// derived from the bind address + port. When the port is OS-assigned
// (0 / random) the absolute base is left empty: the in-app toast does
// not need it, and a wrong absolute link is worse than none.
func alertSettingsFromEnv(bind string, port int) *runview.AlertSettings {
	set := &runview.AlertSettings{
		WebhookURL:   os.Getenv("ITERION_ALERTS_WEBHOOK_URL"),
		StallTimeout: alert.DefaultStallTimeout,
	}
	if v := os.Getenv("ITERION_ALERTS_STALL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			set.StallTimeout = d
		}
	}
	if base := os.Getenv("ITERION_ALERTS_BASE_URL"); base != "" {
		set.BaseURL = base
	} else if port > 0 {
		host := bind
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		set.BaseURL = fmt.Sprintf("http://%s:%d", host, port)
	}
	return set
}

// desktopAlertsEnabled reports whether native desktop notifications are
// turned on via ITERION_ALERTS_DESKTOP_ENABLED (the env mirror of the
// alerts.desktop.enabled config). Default false: the desktop sink is
// opt-in so headless / browser-only sessions never pay for a Wails emit
// that has no consumer.
func desktopAlertsEnabled() bool {
	b, err := strconv.ParseBool(os.Getenv("ITERION_ALERTS_DESKTOP_ENABLED"))
	return err == nil && b
}

// RunStudio starts the studio HTTP server.
//
// Port semantics:
//   - opts.Port == 0  : default to 4891 (legacy CLI behaviour — the
//     `iterion studio` cobra command relies on this when --port is omitted).
//   - opts.Port == -1 : let the OS pick a free port. The actual bind address
//     is delivered via opts.OnReady. Used by the desktop host so multiple
//     instances/projects never fight for 4891.
//   - opts.Port > 0   : use that exact port.
//
// isLoopbackBindHost reports whether bind addresses only the loopback
// interface, so the no-auth local studio is reachable only from this host. An
// unparseable hostname is treated as non-loopback (fail safe).
func isLoopbackBindHost(bind string) bool {
	b := strings.TrimSpace(bind)
	if b == "" || strings.EqualFold(b, "localhost") {
		return true
	}
	if ip := net.ParseIP(b); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func RunStudio(ctx context.Context, opts StudioOptions, p *Printer) error {
	switch {
	case opts.Port == 0:
		opts.Port = 4891
	case opts.Port == -1:
		opts.Port = 0
	}
	if opts.Bind == "" {
		opts.Bind = "127.0.0.1"
	}

	// Local mode trusts the TTY user over loopback. Binding to a non-loopback
	// address (e.g. --bind 0.0.0.0) exposes the no-auth surface to the network:
	// any reachable host can curl /api/* with no Origin header — which the CSRF
	// gate intentionally lets through (non-browser caller) — and gets a
	// synthesized super-admin, so launching a bot/tool node is host-shell RCE.
	// Refuse unless the operator explicitly accepts the risk (e.g. auth added by
	// a fronting reverse proxy / VPN), mirroring the SSRF/SSO non-loopback
	// strictness elsewhere in the server.
	disableAuth := true
	if opts.Mode != "cloud" && !isLoopbackBindHost(opts.Bind) {
		if os.Getenv("ITERION_STUDIO_INSECURE_NONLOOPBACK") != "1" {
			return fmt.Errorf("refusing to start an unauthenticated studio on non-loopback bind %q: any reachable host gets unauthenticated super-admin (launching a bot/tool node = host RCE). Bind 127.0.0.1 (default), or front it with auth and set ITERION_STUDIO_INSECURE_NONLOOPBACK=1 to accept the risk", opts.Bind)
		}
		p.Line("⚠️  studio: bound to non-loopback %q with auth disabled — unauthenticated network access is possible (ITERION_STUDIO_INSECURE_NONLOOPBACK=1)", opts.Bind)
	}

	dir := opts.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}

	// On-disk source for the studio's quick-open "Bots" panel + the
	// embedded-recipe load override. Prefer the productised bots/ dir
	// (where the team now lives); fall back to the legacy examples/ dir
	// for repos that still keep workflows there.
	examplesDir := filepath.Join(dir, "bots")
	if _, err := os.Stat(examplesDir); err != nil {
		examplesDir = filepath.Join(dir, "examples")
		if _, err := os.Stat(examplesDir); err != nil {
			examplesDir = ""
		}
	}

	// Resolve the bot-discovery roots for the boot-scoped consumers (the
	// embedded dispatcher Manager and the trigger-spine scan below). When
	// the operator didn't pass --bots-path, fall back to the conventional
	// <dir>/{bots,examples,.botz} default — the same set the server's
	// effectivePaths() uses. Without this, DefaultBotsPaths received raw
	// nil and the dispatcher could resolve no catalog bot, silently
	// running the default workflow for every explicit-bot ticket.
	//
	// The HTTP server's cfg.Bots.Paths deliberately receives only the
	// EXPLICIT --bots-path values (below): when unset, effectivePaths()
	// re-derives the defaults from the live WorkDir on every request, so
	// /api/v1/bots (and pipeline-ticket bot resolution) follows a studio
	// project switch instead of staying pinned to the boot workspace.
	botsPaths := opts.BotsPaths
	if len(botsPaths) == 0 {
		botsPaths = botregistry.DefaultPaths(dir)
	}

	cfg := server.Config{
		Port:                   opts.Port,
		Bind:                   opts.Bind,
		ExamplesDir:            examplesDir,
		WorkDir:                dir,
		StoreDir:               opts.StoreDir,
		OpenBrowser:            !opts.NoBrowser,
		Mode:                   opts.Mode,
		MaxUploadSize:          opts.MaxUploadSize,
		MaxTotalUploadSize:     opts.MaxTotalUploadSize,
		MaxUploadsPerRun:       opts.MaxUploadsPerRun,
		AllowedUploadMIMEs:     opts.AllowUploadMime,
		MaxConcurrentPipelines: opts.MaxConcurrentPipelines,
		// Local mode: the studio process is implicitly trusted to
		// its TTY user. CSRF protection still gates write endpoints
		// via Origin allowlisting; cross-tenant isolation does not
		// apply because there is exactly one local user.
		DisableAuth: disableAuth,
		Bots:        server.BotsConfig{Paths: opts.BotsPaths},
		Alerts:      alertSettingsFromEnv(opts.Bind, opts.Port),
	}
	// Native desktop notifications: wire the Wails-backed sink the desktop
	// host supplied, but only when the operator opted in. Browser/headless
	// sessions still get the always-on browser toast + dot regardless.
	if opts.DesktopAlertSink != nil && desktopAlertsEnabled() && cfg.Alerts != nil {
		cfg.Alerts.DesktopSink = opts.DesktopAlertSink
	}
	// Wire the in-memory BrowserRegistry unless the operator
	// explicitly disabled the pane. The registry is process-local;
	// the runtime + iterion __browser-attach command share it via
	// the Server reference exposed below.
	if !opts.NoBrowserPane {
		cfg.BrowserRegistry = mcp.NewMemoryBrowserRegistry()
	}

	// Open the native kanban tracker eagerly so the studio's Board
	// view works without a separately-running `iterion dispatch`. The
	// store lives at <store-dir>/dispatcher/ and is auto-initialized
	// with the default board on first use.
	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)
	resolvedStoreDir := store.ResolveStoreDir(dir, opts.StoreDir)

	// Local (non-cloud) secret store: a machine-global sealed file
	// (~/.iterion/secrets.json) plus an optional per-project override
	// (<store-dir>/secrets.json), sealed with a keychain/keyfile master key.
	// Wiring both the store and its sealer enables the studio Secrets view +
	// /api/local/secrets and lets in-process runs resolve declared secrets.
	if opts.Mode != "cloud" {
		if lstore, lerr := LocalSecretStores(resolvedStoreDir); lerr != nil {
			logger.Warn("studio: local secrets disabled (%v)", lerr)
		} else {
			// Lazy sealer: the master key (keychain entry / keyfile) is minted
			// only on the first secret Seal/Open, so merely launching studio
			// never creates key material for an operator who doesn't use secrets.
			cfg.GenericSecrets = lstore
			cfg.Sealer = secrets.NewLazyLocalSealer(store.GlobalIterionDataDir(), logger.Warn)
		}
	}
	ns, nsErr := native.NewStore(filepath.Join(resolvedStoreDir, "dispatcher"))
	if nsErr == nil {
		cfg.NativeTrackerStore = ns
		// A Manager sits idle alongside the native store. The SPA can
		// configure + start + pause + stop the dispatcher entirely
		// from the Board / Dispatcher views; no separate `iterion
		// dispatch` process required.
		mgr, mgrErr := dispatcher.NewManager(dispatcher.ManagerOptions{
			StoreDir:         resolvedStoreDir,
			NativeStore:      ns,
			Logger:           logger,
			DefaultBotsPaths: botsPaths,
			DefaultsFn: func() (*dispatcher.Config, error) {
				// Pass the studio's working directory as the
				// project seed. The auto-config installs an
				// after_create hook that `git worktree add`s
				// from this path so per-issue workspaces are
				// populated with the host checkout instead of
				// landing on the bot as empty dirs (causing
				// scan_docs to see doc_count=0 and the
				// downstream agentic loop to operate against
				// nothing).
				return BuildDefaultConfig(resolvedStoreDir, dir)
			},
		})
		if mgrErr == nil {
			cfg.Dispatcher = mgr
		} else {
			logger.Warn("studio: dispatcher manager init: %v", mgrErr)
		}
		// Activate the event-driven trigger spine when any discovered bot
		// declares a board: invocation. The server then promotes matching
		// cards the moment they transition (the dispatcher claims them now,
		// not at the next poll). nil = no board triggers → spine stays off.
		cfg.TriggerStore = buildLocalTriggerStore(botsPaths, logger)
	} else {
		// Without the native store, cfg.NativeTrackerStore AND cfg.Dispatcher
		// both stay nil, so the server silently mounts neither the /board nor
		// the /dispatcher surface (server_info reports them disabled). Left
		// unlogged this looks like "the Board tab is just empty/broken" with
		// no cause — the operator's whole loop starts at the board. Surface
		// the reason (typically a corrupt or unwritable
		// <store>/dispatcher/board.json) so it's actionable. Non-fatal: the
		// studio still serves the editor / run console for everything else.
		logger.Warn("studio: native tracker store init failed: %v — Board and Dispatcher views are disabled this session (check permissions / integrity of %s/board.json)", nsErr, filepath.Join(resolvedStoreDir, "dispatcher"))
	}

	// Open the hosted-marketplace store eagerly so the studio's
	// Marketplace view works without extra wiring. The store lives at
	// <store-dir>/marketplace/marketplace.json; init failure is non-fatal
	// (the view stays hidden via MarketplaceEnabled=false).
	if mp, mpErr := marketplace.NewJSONStore(filepath.Join(resolvedStoreDir, "marketplace")); mpErr == nil {
		cfg.Marketplace = mp
		// Seed the registry from the workspace's own bot bundles so the
		// Marketplace view isn't empty on first open. Idempotent + best
		// effort; user-submitted entries are never clobbered. Path is
		// configurable (ITERION_MARKETPLACE_SEED_PATHS, comma-separated,
		// workspace-relative) so this stays repo-agnostic — it surfaces
		// whatever bots the target repo ships, defaulting to bots/.
		seedPaths := marketplaceSeedPaths()
		if n, sErr := SeedMarketplace(context.Background(), mp, SeedOptions{Paths: seedPaths, Workdir: dir}); sErr != nil {
			logger.Warn("studio: marketplace seed failed: %v", sErr)
		} else if n > 0 {
			logger.Info("studio: seeded %d built-in bot(s) into the marketplace", n)
		}
	} else {
		logger.Warn("studio: marketplace store init failed: %v — Marketplace view disabled this session", mpErr)
	}

	srv := server.New(cfg, logger)
	srv.OnForceRefresh = opts.OnForceRefresh

	// Open browser in background — only meaningful when port is fixed
	// upfront. For Port=0 (random) callers should use OnReady.
	if !opts.NoBrowser && opts.Port != 0 {
		go openBrowser(fmt.Sprintf("http://localhost:%d", opts.Port))
	}

	if p.Format == OutputHuman && opts.Port != 0 {
		p.Header("Iterion Studio")
		p.KV("URL", fmt.Sprintf("http://localhost:%d", opts.Port))
		if examplesDir != "" {
			p.KV("Bots", examplesDir)
		}
		p.Blank()
		p.Line("  Press Ctrl+C to stop.")
	}

	// Run server until context is cancelled.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	// Notify the caller once the listener is bound. Addr() blocks until
	// then. We run this in its own goroutine to avoid stalling shutdown
	// if the listener never comes up — the addrReady channel is closed
	// on bind failure too, so OnReady fires with the empty string and
	// the caller can check.
	if opts.OnReady != nil {
		go func() {
			addr := srv.Addr()
			if addr != "" && opts.OnReady != nil {
				opts.OnReady(addr)
			}
		}()
	}

	select {
	case <-ctx.Done():
		// 60 s drain budget: long enough to let in-flight runs reach
		// their cancel points + flip persisted status to
		// failed_resumable, but bounded so a wedged subprocess can't
		// hold the studio process forever. Server.Shutdown calls
		// runs.Drain (which uses this ctx) and then http.Server.Shutdown
		// in sequence, so the budget is shared across both phases.
		if p.Format == OutputHuman {
			p.Line("\nShutdown signal received — draining in-flight runs (up to 60s)…")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		// Reap the child to avoid a zombie process for the lifetime
		// of `iterion studio`. xdg-open / open / rundll32 typically
		// exit within milliseconds after spawning the actual browser.
		go func() { _ = cmd.Wait() }()
	}
}
