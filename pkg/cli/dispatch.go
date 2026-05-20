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

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// RunDispatch loads the config, opens the necessary stores, builds a
// Manager + starts a dispatcher, then serves the REST/WS surface until
// SIGINT/SIGTERM.
//
// This is the standalone CLI path — same Manager primitive that the
// studio server uses, just driven by a YAML on disk instead of the
// SPA's PUT /api/v1/dispatcher/config endpoint.
func RunDispatch(p *Printer, opts DispatchOptions) error {
	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)

	var cfg *dispatcher.Config
	if opts.ConfigPath == "" {
		cfg, err = BuildDefaultConfig(storeDir)
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
	if err := mgr.Start(); err != nil {
		return err
	}
	defer mgr.Stop()

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

	port := pickPort(cfg, opts)
	var httpSrv *http.Server
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
				cancel()
			}
		}()
	} else if p.Format == OutputHuman {
		p.Line("iterion dispatch: HTTP disabled (cfg.server.port=0)")
	}

	if p.Format == OutputHuman {
		if opts.ConfigPath == "" {
			p.Line("iterion dispatch: running with baked-in defaults (no YAML) — write iterion.dispatcher.yaml + pass it as argument to customise")
			p.Line("iterion dispatch: assignee bots: %v", DefaultAssigneeNames())
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
