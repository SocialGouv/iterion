package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/store"
)

// DispatchOptions captures CLI flags for `iterion dispatch`.
type DispatchOptions struct {
	ConfigPath string
	StoreDir   string
	Port       int  // overrides cfg.Server.Port if > 0
	NoServer   bool // overrides cfg.Server.Port to disable HTTP
}

// dispatchLogConfig resolves the log configuration of the dispatcher
// daemon. It is a production surface like `iterion server` and
// `iterion runner`, so it takes the same default — structured JSON on
// stderr, ready to ship — while the interactive CLI commands keep the
// human format. ITERION_LOG_FORMAT / ITERION_LOG_LEVEL still win.
func dispatchLogConfig() (iterconfig.LogConfig, error) {
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{
		DefaultLogFormat: iterconfig.LogFormatJSON,
	})
	if err != nil {
		return iterconfig.LogConfig{}, err
	}
	return cfg.Log, nil
}

// RunDispatch loads the config, opens the necessary stores, builds a
// Manager + starts a dispatcher, then serves the REST/WS surface until
// SIGINT/SIGTERM.
//
// This is the standalone CLI path — same Manager primitive that the
// studio server uses, just driven by a YAML on disk instead of the
// SPA's PUT /api/v1/dispatcher/config endpoint.
func RunDispatch(p *Printer, opts DispatchOptions) error {
	logCfg, err := dispatchLogConfig()
	if err != nil {
		return err
	}
	logger := logCfg.NewLogger(os.Stderr)

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)

	var cfg *dispatcher.Config
	if opts.ConfigPath == "" {
		// Seed per-issue workspaces from cwd (the user's project at
		// `iterion dispatch` invocation time). The default builder
		// installs an after_create hook that `git worktree add`s
		// from this path so bots land on a populated checkout
		// matching the host repo's HEAD.
		cfg, err = BuildDefaultConfig(storeDir, cwd)
	} else {
		cfg, err = dispatcher.Load(opts.ConfigPath)
	}
	if err != nil {
		return err
	}

	nativeStore, err := native.NewStore(filepath.Join(storeDir, "dispatcher"))
	if err != nil {
		return fmt.Errorf("native store: %w", err)
	}
	nativeStore.SetLogger(logger)

	wsRoot := cfg.Workspace.Root
	if wsRoot == "" {
		wsRoot = filepath.Join(storeDir, "dispatcher", "workspaces")
	}
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		return fmt.Errorf("dispatcher: mkdir workspace root: %w", err)
	}
	lockPath := filepath.Join(wsRoot, ".dispatcher.lock")
	lk, err := store.AcquireFileLock(lockPath, "dispatcher workspace "+wsRoot)
	if err != nil {
		return err
	}
	defer func() {
		if err := lk.Unlock(); err != nil {
			logger.Warn("dispatcher: unlock %s: %v", lockPath, err)
		}
	}()

	mgr, err := dispatcher.NewManager(dispatcher.ManagerOptions{
		StoreDir:    storeDir,
		NativeStore: nativeStore,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	if err := mgr.SaveConfig(cfg); err != nil {
		return err
	}
	// NewManager auto-starts when the store carries a persisted config
	// (first-boot intent defaults to Running), so on any re-run of
	// `iterion dispatch` over the same store the explicit Start finds a
	// live instance. That IS the desired end state — SaveConfig above
	// already live-reloaded it with this invocation's config — so treat
	// ErrAlreadyRunning as success instead of exiting the daemon.
	if err := mgr.Start(); err != nil {
		if !errors.Is(err, dispatcher.ErrAlreadyRunning) {
			return err
		}
		logger.Info("dispatcher already auto-started from the persisted config — this invocation's config was live-reloaded onto it")
	}
	// Shutdown preserves the operator's last-known intent in
	// runtime.json across SIGTERM / Ctrl-C. Stop is reserved for
	// operator-driven shutdown via the HTTP API.
	defer mgr.Shutdown()

	// In zero-config mode there is no YAML on disk to watch — operators
	// who want live reloads should write a config file and pass it as
	// an argument. Skipping the watcher avoids a noisy startup warning
	// from fsnotify against an empty path.
	if opts.ConfigPath != "" {
		watcher, err := dispatcher.NewConfigWatcher(opts.ConfigPath, logger)
		if err != nil {
			return err
		}
		if err := watcher.Start(func(c *dispatcher.Config) {
			if err := mgr.SaveConfig(c); err != nil {
				logger.Warn("dispatcher: reload: %v", err)
			}
		}); err != nil {
			return err
		}
		defer watcher.Stop()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Event-driven trigger spine: when any discovered bot declares a board:
	// invocation, promote matching cards the moment they transition so the
	// dispatcher claims them now instead of at the next poll. nil store =
	// spine off (poll-only). The Manager doubles as the nudger.
	if ts := buildLocalTriggerStore(cfg.Bots.Paths, logger); ts != nil {
		if tc := server.StartTriggerCoordinator(nativeStore, ts, mgr, nil, nil, nil, logger); tc != nil {
			defer tc.Close()
		}
	}

	port := pickPort(cfg, opts)
	var httpSrv *http.Server
	// Buffered so the HTTP goroutine's error send never blocks even when
	// the daemon is torn down by a signal before we read it. Checked after
	// shutdown so a bind failure (port already in use, permission denied)
	// propagates a non-zero exit instead of a misleading exit 0 — without
	// it, a supervisor (systemd/k8s) keyed on exit code treats a daemon
	// whose operator-facing API never came up as a clean stop and won't
	// restart it, leaving the dispatcher silently unreachable.
	httpErrCh := make(chan error, 1)
	if port > 0 {
		mux := http.NewServeMux()
		mgr.RegisterRoutes(mux, "/api/v1/dispatcher")
		nativeStore.RegisterRoutes(mux, "/api/v1/native")
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		mux.HandleFunc("/api/server/info", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mode":"dispatch","auth_required":false,"limits":{"upload":{}},"native_tracker_enabled":true,"dispatcher_enabled":true}`))
		})
		if sub, err := fs.Sub(server.StaticFS, "static"); err == nil {
			mux.Handle("/", server.SPAHandler(sub))
		} else {
			logger.Warn("dispatcher: SPA assets not available: %v", err)
		}

		httpSrv = &http.Server{
			Addr:              fmt.Sprintf("127.0.0.1:%d", port),
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		if p.Format == OutputHuman {
			p.Line("iterion dispatch: HTTP on http://%s", httpSrv.Addr)
		}
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("dispatcher http server: %v", err)
				httpErrCh <- err
				cancel()
			}
		}()
	} else if p.Format == OutputHuman {
		p.Line("iterion dispatch: HTTP disabled (cfg.server.port=0)")
	}

	if p.Format == OutputHuman {
		if opts.ConfigPath == "" {
			p.Line("iterion dispatch: running with baked-in defaults (no YAML) — write iterion.dispatcher.yaml + pass it as argument to customise")
			p.Line("iterion dispatch: assignee bots: %v", DefaultAssigneeNames(cfg.Bots.Paths))
		}
		p.Line("iterion dispatch: workflow %s", cfg.Workflow)
		p.Line("iterion dispatch: tracker %s", cfg.Tracker.Kind)
		p.Line("iterion dispatch: workspaces under %s", wsRoot)
		p.Line("iterion dispatch: polling every %s, stall timeout %s", cfg.PollingInterval(), cfg.StallTimeout())
		p.Line("iterion dispatch: ctrl-c to stop")
	}

	<-ctx.Done()
	if p.Format == OutputHuman {
		p.Line("iterion dispatch: shutting down…")
	}
	if httpSrv != nil {
		shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(shutdownCtx)
		sc()
	}
	// Distinguish a signal-driven stop (clean, exit 0) from a teardown the
	// HTTP server forced via cancel() (bind failure → non-zero exit). A
	// graceful Shutdown returns http.ErrServerClosed, which the goroutine
	// filters out, so the channel only ever holds a genuine serve error.
	select {
	case err := <-httpErrCh:
		return fmt.Errorf("dispatcher: http server failed: %w", err)
	default:
	}
	return nil
}

func pickPort(cfg *dispatcher.Config, opts DispatchOptions) int {
	if opts.NoServer {
		return 0
	}
	if opts.Port > 0 {
		return opts.Port
	}
	return cfg.Server.Port
}
