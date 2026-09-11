package cli

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/modelprefs"
	"github.com/SocialGouv/iterion/pkg/projectenv"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	"github.com/SocialGouv/iterion/pkg/store"
)

func runWorkspaceStudio(ctx context.Context, opts StudioOptions, p *Printer) error {
	logger := iterlog.NewFromEnv(os.Stderr)
	errtrack.Init(errtrack.Config{Logger: logger, ServerName: "iterion-workspace"})
	errtrack.AttachLogHook(logger)
	defer errtrack.Flush()

	registry, err := projects.Load()
	if err != nil {
		return fmt.Errorf("workspace: load projects: %w", err)
	}
	if err := importLegacyInstances(registry, logger); err != nil {
		return err
	}
	if opts.Dir != "" {
		if err := ensureWorkspaceProject(registry, opts); err != nil {
			return err
		}
	}
	if err := registry.Save(); err != nil {
		return fmt.Errorf("workspace: save projects: %w", err)
	}
	if len(registry.RecentProjects) == 0 {
		return fmt.Errorf("workspace: no projects are registered; pass --dir to add one")
	}

	host, err := server.NewWorkspaceHost(registry, func(project projects.Project) (*server.Server, error) {
		return newWorkspaceProjectServer(opts, project, logger)
	}, logger)
	if err != nil {
		return err
	}
	teardown, err := iterconfig.ShutdownTeardownFromEnv(60 * time.Second)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = host.Shutdown(shutdownCtx)
		cancel()
		return err
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(opts.Bind, strconv.Itoa(opts.Port)))
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = host.Shutdown(shutdownCtx)
		cancel()
		return err
	}
	actualAddr := listener.Addr().String()
	public := &http.Server{
		Handler:           host.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- public.Serve(listener) }()
	if opts.OnReady != nil {
		opts.OnReady(actualAddr)
	}
	url := workspaceURL(opts.Bind, listener.Addr())
	if !opts.NoBrowser {
		go openBrowser(url)
	}
	if p.Format == OutputHuman {
		p.Header("Iterion Workspace")
		p.KV("URL", url)
		p.KV("Projects", fmt.Sprintf("%d registered", len(registry.RecentProjects)))
		p.Blank()
		p.Line("  Press Ctrl+C to stop.")
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), teardown)
		defer cancel()
		httpErr := public.Shutdown(shutdownCtx)
		runtimeErr := host.Shutdown(shutdownCtx)
		if httpErr != nil {
			return httpErr
		}
		return runtimeErr
	case serveErr := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), teardown)
		defer cancel()
		_ = host.Shutdown(shutdownCtx)
		if serveErr == http.ErrServerClosed {
			return nil
		}
		return serveErr
	}
}

func workspaceURL(bind string, addr net.Addr) string {
	host := bind
	if host == "" || host == "127.0.0.1" || host == "::1" {
		host = "localhost"
	}
	port := ""
	if tcp, ok := addr.(*net.TCPAddr); ok {
		port = strconv.Itoa(tcp.Port)
	}
	return "http://" + net.JoinHostPort(host, port)
}

func ensureWorkspaceProject(registry *projects.Config, opts StudioOptions) error {
	root, err := filepath.Abs(opts.Dir)
	if err != nil {
		return fmt.Errorf("workspace: project root: %w", err)
	}
	storeDir := store.ResolveStoreDir(root, opts.StoreDir)
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return fmt.Errorf("workspace: create store: %w", err)
	}
	envFile := ""
	if fileExists(filepath.Join(root, ".env")) {
		envFile = filepath.Join(root, ".env")
	}
	botsPaths := opts.BotsPaths
	if len(botsPaths) == 0 {
		botsPaths = botregistry.DefaultPaths(root)
	}
	_, _, err = registry.RegisterWithStore(root, storeDir, botsPaths, envFile)
	return err
}

func newWorkspaceProjectServer(opts StudioOptions, project projects.Project, logger *iterlog.Logger) (*server.Server, error) {
	runEnv, err := projectenv.Snapshot(os.Environ(), project.EnvFile)
	if err != nil {
		return nil, err
	}
	botsPaths := project.BotsPaths
	if len(botsPaths) == 0 {
		botsPaths = botregistry.DefaultPaths(project.Dir)
	}
	examplesDir := filepath.Join(project.Dir, "bots")
	if !dirExists(examplesDir) {
		examplesDir = filepath.Join(project.Dir, "examples")
		if !dirExists(examplesDir) {
			examplesDir = ""
		}
	}
	shutdownDelay, err := iterconfig.ShutdownDelayFromEnv(0)
	if err != nil {
		return nil, err
	}
	cfg := server.Config{
		Bind:                    opts.Bind,
		Port:                    opts.Port,
		ExamplesDir:             examplesDir,
		WorkDir:                 project.Dir,
		StoreDir:                project.StoreDir,
		RunEnv:                  runEnv,
		Mode:                    opts.Mode,
		DisableAuth:             true,
		Bots:                    server.BotsConfig{Paths: botsPaths},
		BotSources:              botsource.NewMemoryStore(),
		Alerts:                  alertSettingsFromEnv(opts.Bind, opts.Port),
		RecoveryPassive:         opts.RecoveryPassive,
		SkipProjectRegistration: true,
		MaxUploadSize:           opts.MaxUploadSize,
		MaxTotalUploadSize:      opts.MaxTotalUploadSize,
		MaxUploadsPerRun:        opts.MaxUploadsPerRun,
		AllowedUploadMIMEs:      opts.AllowUploadMime,
		MaxConcurrentPipelines:  opts.MaxConcurrentPipelines,
		ShutdownDelay:           shutdownDelay,
	}
	if opts.DesktopAlertSink != nil && desktopAlertsEnabled() && cfg.Alerts != nil {
		cfg.Alerts.DesktopSink = opts.DesktopAlertSink
	}
	if !opts.NoBrowserPane {
		cfg.BrowserRegistry = mcp.NewMemoryBrowserRegistry()
	}
	if localStores, storeErr := LocalSecretStores(project.StoreDir); storeErr != nil {
		logger.Warn("workspace: local secrets disabled for %s: %v", project.Name, storeErr)
	} else {
		cfg.GenericSecrets = localStores
		cfg.Sealer = secrets.NewLazyLocalSealer(store.GlobalIterionDataDir(), logger.Warn)
	}
	prefs := modelprefs.NewFileStore(project.StoreDir)
	prefs.SetLogger(logger)
	cfg.ModelPrefs = prefs

	nativeStore, nativeErr := native.NewStore(filepath.Join(project.StoreDir, "dispatcher"))
	if nativeErr != nil {
		logger.Warn("workspace: board disabled for %s: %v", project.Name, nativeErr)
	} else {
		nativeStore.SetLogger(logger)
		cfg.NativeTrackerStore = nativeStore
		if !opts.RecoveryPassive {
			manager, managerErr := dispatcher.NewManager(dispatcher.ManagerOptions{
				StoreDir:         project.StoreDir,
				NativeStore:      nativeStore,
				Logger:           logger,
				DefaultBotsPaths: botsPaths,
				RunEnv:           runEnv,
				DefaultsFn: func() (*dispatcher.Config, error) {
					return BuildDefaultConfig(project.StoreDir, project.Dir)
				},
			})
			if managerErr != nil {
				logger.Warn("workspace: dispatcher disabled for %s: %v", project.Name, managerErr)
			} else {
				cfg.Dispatcher = manager
			}
			cfg.TriggerStore = buildLocalTriggerStore(botsPaths, logger)
		}
	}
	if !opts.RecoveryPassive {
		if market, marketErr := marketplace.NewJSONStore(filepath.Join(project.StoreDir, "marketplace")); marketErr == nil {
			cfg.Marketplace = market
			if _, seedErr := SeedMarketplace(context.Background(), market, SeedOptions{Paths: marketplaceSeedPaths(), Workdir: project.Dir}); seedErr != nil {
				logger.Warn("workspace: marketplace seed failed for %s: %v", project.Name, seedErr)
			}
		} else {
			logger.Warn("workspace: marketplace disabled for %s: %v", project.Name, marketErr)
		}
	}
	srv := server.New(cfg, logger)
	srv.OnForceRefresh = opts.OnForceRefresh
	return srv, nil
}

func importLegacyInstances(registry *projects.Config, logger *iterlog.Logger) error {
	configPath := os.Getenv("ITERION_INSTANCES_CONFIG")
	if configPath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return nil
		}
		configPath = filepath.Join(configDir, "iterion", "instances.conf")
	}
	f, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("workspace: open legacy instances: %w", err)
	}
	defer f.Close()

	loadProjectEnv := true
	currentID := registry.CurrentProjectID
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "LOAD_PROJECT_ENV=") {
			value := strings.Trim(strings.TrimPrefix(line, "LOAD_PROJECT_ENV="), "\"'")
			if parsed, parseErr := strconv.ParseBool(value); parseErr == nil {
				loadProjectEnv = parsed
			}
			continue
		}
		if strings.Contains(strings.Fields(line)[0], "=") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		root := expandHome(fields[1])
		if !dirExists(root) {
			logger.Warn("workspace: legacy project %s unavailable at %s", fields[0], root)
			continue
		}
		storeDir := ""
		botsPaths := []string{}
		noEnv := !loadProjectEnv
		for i := 3; i < len(fields); i++ {
			switch {
			case fields[i] == "local-store":
				storeDir = filepath.Join(root, ".iterion")
			case fields[i] == "no-env":
				noEnv = true
			case fields[i] == "--store-dir" && i+1 < len(fields):
				i++
				storeDir = expandHome(fields[i])
			case strings.HasPrefix(fields[i], "--store-dir="):
				storeDir = expandHome(strings.TrimPrefix(fields[i], "--store-dir="))
			case fields[i] == "--bots-path" && i+1 < len(fields):
				i++
				botsPaths = append(botsPaths, expandHome(fields[i]))
			case strings.HasPrefix(fields[i], "--bots-path="):
				botsPaths = append(botsPaths, expandHome(strings.TrimPrefix(fields[i], "--bots-path=")))
			}
		}
		if storeDir == "" {
			storeDir = store.ResolveStoreDir(root, "")
		}
		if !dirExists(storeDir) {
			logger.Warn("workspace: legacy store for %s unavailable at %s", fields[0], storeDir)
			continue
		}
		if len(botsPaths) == 0 {
			botsPaths = botregistry.DefaultPaths(root)
		}
		envFile := ""
		if !noEnv && fileExists(filepath.Join(root, ".env")) {
			envFile = filepath.Join(root, ".env")
		}
		if _, _, registerErr := registry.RegisterWithStore(root, storeDir, botsPaths, envFile); registerErr != nil {
			return fmt.Errorf("workspace: import legacy project %s: %w", fields[0], registerErr)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("workspace: read legacy instances: %w", err)
	}
	if currentID != "" {
		registry.SetCurrent(currentID)
	}
	return nil
}

func expandHome(value string) string {
	value = strings.Trim(value, "\"'")
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	return value
}

func dirExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.IsDir()
}

func fileExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular()
}
