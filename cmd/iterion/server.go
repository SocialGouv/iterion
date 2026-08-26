package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SocialGouv/iterion/pkg/audit"
	iterauth "github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/cloud/orgsweep"
	"github.com/SocialGouv/iterion/pkg/cloud/tracing"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/mail"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usagecap"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	usernotifywebpush "github.com/SocialGouv/iterion/pkg/usernotify/webpush"
	"github.com/SocialGouv/iterion/pkg/valkey"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// `iterion server` is the cloud-mode HTTP server entry point. In
// local mode it delegates to cli.RunStudio (same handler tree as
// `iterion studio`). In cloud mode it builds a Mongo+S3 store + a
// NATS-backed LaunchPublisher and feeds them into pkg/server.Server
// so handleLaunchRun publishes to the queue instead of spawning the
// runtime in-process.
//
// Differences from `iterion studio` regardless of mode:
//   - default --bind is 0.0.0.0 (cloud pods need LAN exposure);
//   - --no-browser is forced on (no display in a container).
//
// Cloud-ready plan §F (T-30, T-31, T-32, T-33).

var serverOpts struct {
	port       int
	bind       string
	dir        string
	storeDir   string
	configPath string
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the iterion HTTP server (studio + run console + cloud API)",
	Long: `iterion server is the cloud-deployment HTTP entry point. It serves the
studio, the run console (REST + WebSocket), and the launch /
resume / cancel API on a single port. Health endpoints (/healthz,
/readyz) live alongside the API.

Mode is chosen by ITERION_MODE:
  - local (default): in-process engine; same as 'iterion studio'.
  - cloud: persists to Mongo+S3, publishes runs onto NATS for the
    runner pool to consume.

For local dev, prefer 'iterion studio' which keeps the loopback bind
default and opens the browser.`,
	Args: cobra.NoArgs,
	RunE: runServer,
}

func init() {
	f := serverCmd.Flags()
	f.IntVar(&serverOpts.port, "port", 4891, "HTTP port")
	f.StringVar(&serverOpts.bind, "bind", "0.0.0.0", "Bind address (default 0.0.0.0 for cloud pods)")
	f.StringVar(&serverOpts.dir, "dir", "", "Working directory")
	f.StringVar(&serverOpts.storeDir, "store-dir", "", "Run store directory (local mode only)")
	f.StringVar(&serverOpts.configPath, "config", "", "Path to YAML config (env vars take precedence)")
	serverCmd.AddCommand(webpushKeysCmd)
	rootCmd.AddCommand(serverCmd)
}

// webpushKeysCmd mints the VAPID keypair browser push notifications ride
// on. Run once per deployment; every server replica must share the SAME
// pair (rotating it invalidates all stored browser subscriptions).
var webpushKeysCmd = &cobra.Command{
	Use:   "webpush-keys",
	Short: "Generate a VAPID keypair for browser push notifications",
	Long: `Generate the VAPID keypair web-push notifications are signed with.

Store both values in the server environment (e.g. the deploy secret):
every replica must share the one pair, and rotating it invalidates every
registered browser subscription (users just re-enable notifications in
their settings).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		priv, pub, err := usernotifywebpush.GenerateVAPIDKeys()
		if err != nil {
			return fmt.Errorf("generate VAPID keys: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "ITERION_WEBPUSH_VAPID_PUBLIC_KEY=%s\nITERION_WEBPUSH_VAPID_PRIVATE_KEY=%s\n", pub, priv)
		return nil
	},
}

// randomBootstrapPassword returns a URL-safe random temporary password for the
// bootstrap super-admin. base64 of 18 bytes (~24 chars) is comfortably above
// the MinPasswordLen the rotation endpoint enforces.
// orgLimitDefaultsFromEnv reads the platform-wide launch limits
// applied to teams without a per-org override. Unset / invalid /
// zero values mean "no limit" — the safe default for existing
// deployments. Per-org overrides live on the Team document and are
// managed via PATCH /api/admin/orgs/{id}.
func orgLimitDefaultsFromEnv() server.OrgLimitDefaults {
	intEnv := func(key string) int {
		n, err := strconv.Atoi(os.Getenv(key))
		if err != nil || n < 0 {
			return 0
		}
		return n
	}
	var d server.OrgLimitDefaults
	d.MonthlyRunQuota = intEnv("ITERION_ORG_DEFAULT_MONTHLY_RUN_QUOTA")
	d.MaxConcurrentRuns = intEnv("ITERION_ORG_DEFAULT_MAX_CONCURRENT_RUNS")
	d.LaunchRatePerMin = intEnv("ITERION_ORG_DEFAULT_LAUNCH_RATE_PER_MIN")
	if f, err := strconv.ParseFloat(os.Getenv("ITERION_ORG_DEFAULT_MONTHLY_COST_CAP_USD"), 64); err == nil && f > 0 {
		d.MonthlyCostCapUSD = f
	}
	return d
}

// forgeGitHubAppFromEnv reads the GitHub-App identity for the
// installation-token connect mode. The PEM private key is loaded from a file
// (the canonical k8s-secret mount), falling back to an inline env value.
// Empty AppID → the App mode is unavailable (OAuth/PAT still work).
func forgeGitHubAppFromEnv() server.ForgeGitHubAppConfig {
	appID, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv("ITERION_FORGE_GITHUB_APP_ID")), 10, 64)
	key := strings.TrimSpace(os.Getenv("ITERION_FORGE_GITHUB_APP_PRIVATE_KEY"))
	if path := strings.TrimSpace(os.Getenv("ITERION_FORGE_GITHUB_APP_PRIVATE_KEY_FILE")); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			key = string(b)
		}
	}
	return server.ForgeGitHubAppConfig{
		AppID:      appID,
		PrivateKey: key,
		AppSlug:    strings.TrimSpace(os.Getenv("ITERION_FORGE_GITHUB_APP_SLUG")),
		// Optional user-auth OAuth creds: when set (+ "Request user
		// authorization during installation" enabled on the App), the install
		// callback verifies installation ownership before minting a token,
		// closing the installation_id IDOR on the shared-app path.
		ClientID:     strings.TrimSpace(os.Getenv("ITERION_FORGE_GITHUB_APP_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("ITERION_FORGE_GITHUB_APP_CLIENT_SECRET")),
	}
}

func randomBootstrapPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func runServer(cmd *cobra.Command, _ []string) error {
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{
		YAMLPath:         serverOpts.configPath,
		DefaultLogFormat: iterconfig.LogFormatJSON,
	})
	if err != nil {
		return fmt.Errorf("server: load config: %w", err)
	}

	// Local mode: keep the existing studio handlers; only difference
	// from `iterion studio` is the cloud-friendly --bind default.
	if cfg.Mode == iterconfig.ModeLocal {
		return cli.RunStudio(cmd.Context(), cli.StudioOptions{
			Port:      serverOpts.port,
			Bind:      serverOpts.bind,
			Dir:       serverOpts.dir,
			StoreDir:  serverOpts.storeDir,
			NoBrowser: true,
		}, newPrinter())
	}

	// Cloud mode: build Mongo+S3 store + NATS publisher + server
	// directly. We bypass cli.RunStudio because it auto-discovers a
	// filesystem store, which doesn't make sense when persistence
	// lives in Mongo.
	logger := cfg.Log.NewLogger(cmd.ErrOrStderr())
	// Couple the process logger to the error tracker (no-op unless
	// SENTRY_DSN is set): every error line becomes an event, every warn
	// line a breadcrumb on the next one. Init already ran at the root.
	errtrack.Init(errtrack.Config{Logger: logger, ServerName: "iterion-server"})
	errtrack.AttachLogHook(logger)
	defer errtrack.Flush()
	logger.Info("server: starting (mode=cloud)")

	rootCtx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	traceShutdown, err := tracing.Init(rootCtx, "iterion-server", logger)
	if err != nil {
		return fmt.Errorf("server: init tracing: %w", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = traceShutdown(shutCtx)
	}()

	natsConn, err := natsq.Connect(rootCtx, natsq.Config{
		URL:                 cfg.NATS.URL,
		StreamName:          cfg.NATS.Stream,
		DLQStream:           cfg.NATS.DLQStream,
		KVBucket:            cfg.NATS.KVBucket,
		MaxAckPending:       cfg.NATS.MaxAckPending,
		AckWait:             cfg.NATS.AckWait,
		SchemaMismatchDelay: cfg.Runner.SchemaMismatchDelay,
		MaxDeliver:          cfg.NATS.MaxDeliver,
		MaxAge:              cfg.NATS.MaxAge,
		DLQMaxAge:           cfg.NATS.DLQMaxAge,
		MaxPayload:          cfg.NATS.MaxPayload,
		Logger:              logger,
	})
	if err != nil {
		return fmt.Errorf("server: connect NATS: %w", err)
	}
	defer natsConn.Close()

	bc, err := newCloudBlob(rootCtx, cfg.S3)
	if err != nil {
		return fmt.Errorf("server: build blob client: %w", err)
	}
	defer func() { _ = bc.Close() }()

	// Server-side store: no NATS lock provider — the server never
	// executes runs, only publishes them. The runner pod is the
	// only place that takes leases.
	st, err := newCloudMongoStore(rootCtx, cfg.Mongo, bc, logger, nil)
	if err != nil {
		return fmt.Errorf("server: build mongo store: %w", err)
	}
	defer closeCloudStoreWithTimeout(st)

	// Prometheus registry: built early so cloudpublisher + runstream
	// + the run-console WS handler all share the same registry.
	mreg := metrics.New()

	// AES-GCM master key for sealing BYOK + OAuth credentials at
	// rest. Built early so the publisher can pick up the BYOK store.
	sealer, err := secrets.NewAESGCMSealerFromBase64(cfg.Auth.SecretsKey)
	if err != nil {
		return fmt.Errorf("server: build sealer: %w", err)
	}

	stores, err := buildCloudStores(rootCtx, st, logger)
	if err != nil {
		return err
	}

	// Seed the hosted marketplace from the image's bot catalog (bots/, or
	// ITERION_MARKETPLACE_SEED_PATHS) so the public Marketplace view lists
	// iterion's first-class bots out of the box. Best-effort + idempotent;
	// user-submitted (git/upload) entries are never clobbered. No-op when
	// the registry is disabled or the catalog isn't shipped in the image.
	if stores.marketplace != nil {
		if n, sErr := cli.SeedMarketplaceDefault(rootCtx, stores.marketplace, serverOpts.dir); sErr != nil {
			logger.Warn("cloud: marketplace seed failed: %v", sErr)
		} else if n > 0 {
			logger.Info("cloud: seeded %d built-in bot(s) into the marketplace", n)
		}
	}

	// The auth stack is built before the publisher so the publisher can
	// resolve team → org for spend attribution (RunMessage.OrgID).
	authStack, err := buildAuthStack(rootCtx, cfg, st, stores, logger)
	if err != nil {
		return err
	}

	// The mutualised credential pool: contributor-lent subscriptions a run
	// with no credential of its own can draw on. One broker shared by the
	// publisher (selection at launch) and the server (abandoned-lease
	// sweeper), so both see the same stores.
	credBroker := credpool.NewBroker(credpool.BrokerConfig{
		Pools:   stores.credPools,
		Pledges: stores.credPledges,
		Leases:  stores.credLeases,
		Ledger:  stores.credLedger,
		OAuth:   stores.oauth,
		APIKeys: stores.apiKeys,
		Sealer:  sealer,
		Logger:  logger,
	})

	pub, err := cloudpublisher.New(cloudpublisher.Config{
		NATS:             natsConn,
		Store:            st,
		MongoColl:        st.RunsCollection(),
		Logger:           logger,
		Metrics:          mreg,
		ApiKeys:          stores.apiKeys,
		GenericSecrets:   stores.genericSecrets,
		BotBindings:      stores.botBindings,
		RunSecrets:       stores.runSecrets,
		Sealer:           sealer,
		OAuthForfait:     stores.oauth,
		ForgeConnections: stores.forgeConn,
		Identity:         authStack.identityStore,
		PluginSources:    newPluginSourceResolver(stores, sealer, logger),
		CredPool:         credBroker,
		SandboxImage:     platformSandboxImageResolver(stores, logger),
	})
	if err != nil {
		return fmt.Errorf("server: build cloud publisher: %w", err)
	}

	// Mongo change-stream source so the WS handler streams runner-pod
	// events AND run_logs chunks (the local broker/buffer would only
	// see this process's writes). ADR-053.
	streamSrc := runstream.NewMongo(st.EventsCollection(), st.RunLogsCollection(), st.RunsCollection(), logger).WithMetrics(mreg)

	disableAuth, _ := strconv.ParseBool(os.Getenv("ITERION_DISABLE_AUTH"))

	if err := bootstrapAdmin(rootCtx, cfg, authStack.identityStore, authStack.authSvc, disableAuth, logger); err != nil {
		return err
	}

	registry := buildOIDCRegistry(cfg)

	if disableAuth {
		logger.Warn("server: ITERION_DISABLE_AUTH set — /api/* endpoints are unauthenticated; do not expose the server publicly")
	}

	// Run-health alerting: webhook (Slack/Discord) + always-on browser
	// toast sink. Deep links use the externally-reachable PublicURL so
	// webhook recipients get a clickable /runs/<id> link. The desktop
	// sink is nil here (cloud pods have no Wails runtime).
	alertSettings := &runview.AlertSettings{
		WebhookURL:   cfg.Alerts.Webhook.URL,
		StallTimeout: cfg.Alerts.StallTimeout,
		BaseURL:      cfg.Auth.PublicURL,
	}

	// User notifications (web push): the shared event spine (NATSBus over
	// the queue's connection — disjoint subject trees on one link) carries
	// the runner pods' run-outcome events to exactly one server replica
	// (queue-group delivery); the Mongo stores hold browser subscriptions,
	// per-user prefs and the sent-episode dedup claims; the sweep seam
	// reconciles episodes the lossy bus dropped. Enabled iff the VAPID
	// keypair is configured (ITERION_WEBPUSH_*).
	// The NATS events bus is the process-wide trigger-event spine: the
	// board trigger evaluator AND the usernotify dispatcher both ride it,
	// so it is built unconditionally (not only when web-push is on).
	var eventsBus eventbus.Bus
	eventsBus, err = eventbus.NewNATSBus(natsConn.NATS(), eventbus.NATSOptions{Logger: logger})
	if err != nil {
		return fmt.Errorf("server: build events bus: %w", err)
	}
	var pushSubs usernotifywebpush.SubscriptionStore
	var notifPrefs usernotify.PrefsStore
	var notifSent usernotify.SentStore
	var notifiableRuns usernotify.ListNotifiableRuns
	if cfg.WebPush.Enabled() {
		subsStore := usernotifywebpush.NewMongoSubscriptionStore(st.DB())
		prefsStore := usernotify.NewMongoPrefsStore(st.DB())
		sentStore := usernotify.NewMongoSentStore(st.DB())
		for name, ensure := range map[string]func(context.Context) error{
			"push subscriptions": subsStore.EnsureSchema,
			"notification prefs": prefsStore.EnsureSchema,
			"sent notifications": sentStore.EnsureSchema,
		} {
			if sErr := ensure(rootCtx); sErr != nil {
				return fmt.Errorf("server: ensure %s schema: %w", name, sErr)
			}
		}
		pushSubs, notifPrefs, notifSent = subsStore, prefsStore, sentStore
		notifiableRuns = func(ctx context.Context, since, before time.Time, limit int) ([]usernotify.RunRef, error) {
			refs, lErr := st.ListNotifiableRuns(ctx, since, before, limit)
			if lErr != nil {
				return nil, lErr
			}
			out := make([]usernotify.RunRef, 0, len(refs))
			for _, ref := range refs {
				out = append(out, usernotify.RunRef{ID: ref.ID, Status: ref.Status, InteractionID: ref.Checkpoint.InteractionID, UpdatedAt: ref.UpdatedAt})
			}
			return out, nil
		}
	}

	// The studio Home "Bots" panel lists first-class bots via /api/examples
	// (an on-disk ExamplesDir walk). In cloud mode the bot catalog ships at
	// the ITERION_BOTS_PATH dir — the same source /api/v1/bots uses — so point
	// ExamplesDir at it; otherwise /api/examples falls back to the 3
	// binary-embedded recipes and the Home shows only feature-dev +
	// whole/branch-improve-loop instead of the full team.
	botsPaths := botsPathsFromEnv()
	examplesDir := ""
	if len(botsPaths) > 0 {
		examplesDir = botsPaths[0]
	}

	// Valkey/Redis for distributed ephemeral state (forge CSRF, board-MCP
	// tokens, auth rate-limit). Required when running >1 replica; nil → the
	// in-memory stores. A configured-but-unreachable Valkey fails startup.
	var redisClient *valkey.Client
	if cfg.Redis.Enabled() {
		redisClient, err = valkey.New(valkey.Options{
			URL:              cfg.Redis.URL,
			SentinelAddrs:    cfg.Redis.SentinelAddrs,
			MasterName:       cfg.Redis.MasterName,
			Password:         cfg.Redis.Password,
			SentinelPassword: cfg.Redis.SentinelPassword,
		})
		if err != nil {
			return fmt.Errorf("server: valkey: %w", err)
		}
		pingCtx, cancelPing := context.WithTimeout(rootCtx, 5*time.Second)
		if err := redisClient.Ping(pingCtx); err != nil {
			cancelPing()
			return fmt.Errorf("server: valkey unreachable: %w", err)
		}
		cancelPing()
		logger.Info("valkey: connected (distributed state enabled)")
	}

	// Org purge sweeper: nightly hard-purge of orgs whose soft-delete grace has
	// elapsed — all team-scoped data across the cloud collections, then the
	// identity cascade. Multi-replica-safe (idempotent; PurgeOrg no-ops once the
	// org is gone).
	var orgPurgeSweeper *orgsweep.Sweeper
	if authStack.authSvc != nil {
		orgPurgeSweeper = &orgsweep.Sweeper{
			Purger: &orgsweep.Purger{
				DB:      st.DB(),
				Store:   authStack.identityStore,
				Cascade: authStack.authSvc.DeleteOrgCascade,
				Logger:  logger,
			},
			HourUTC: 2,
			Logger:  logger,
		}
	}

	srv := server.New(server.Config{
		Port:                   serverOpts.port,
		Bind:                   serverOpts.bind,
		Bots:                   server.BotsConfig{Paths: botsPaths},
		ExamplesDir:            examplesDir,
		WorkDir:                serverOpts.dir,
		Store:                  st,
		CloudBoardFor:          func(tenantID string) native.BoardStore { return boardmongo.New(st.DB(), tenantID) },
		CloudBoardCoordinator:  boardmongo.NewCoordinator(st.DB()),
		TriggerStore:           trigger.NewMongoSubscriptionStore(st.DB()),
		ScheduledBots:          cloudsched.NewMongoStore(st.DB()),
		OrgPurgeSweeper:        orgPurgeSweeper,
		Alerts:                 alertSettings,
		LaunchPublisher:        pub,
		StreamSource:           streamSrc,
		Mode:                   string(iterconfig.ModeCloud),
		AuthService:            authStack.authSvc,
		AuthSigner:             authStack.signer,
		OIDCRegistry:           registry,
		OIDCStates:             stores.oidcState,
		DesktopTickets:         stores.desktopTickets,
		WSTickets:              stores.wsTickets,
		OrgSSO:                 stores.orgSSO,
		OrgDomains:             stores.orgDomain,
		ApiKeys:                stores.apiKeys,
		GenericSecrets:         stores.genericSecrets,
		BotBindings:            stores.botBindings,
		ForgeConnections:       stores.forgeConn,
		ForgeIntegrations:      stores.forgeIntegration,
		ForgeOAuthApps:         stores.forgeOAuthApp,
		ForgeGitHubApp:         forgeGitHubAppFromEnv(),
		PluginSources:          stores.pluginSources,
		BotSources:             stores.botSources,
		BotRolesSettings:       stores.botRoles,
		SandboxSettings:        stores.sandboxCfg,
		WebhookConfigs:         stores.webhooks.Configs,
		WebhookDeliveries:      stores.webhooks.Deliveries,
		WebhookCounter:         stores.webhooks.Counter,
		ConfigShares:           stores.configShares,
		OrgUsage:               stores.orgUsage,
		OrgDefaults:            orgLimitDefaultsFromEnv(),
		CredPoolBroker:         credBroker,
		CredPoolPools:          stores.credPools,
		CredPoolPledges:        stores.credPledges,
		CredPoolLeases:         stores.credLeases,
		CredPoolLedger:         stores.credLedger,
		Audit:                  stores.audit,
		UsageCapSettings:       stores.usageCapSettings,
		Marketplace:            stores.marketplace,
		Redis:                  redisClient,
		PATs:                   stores.pat,
		PATMaxTTL:              patMaxTTLFromEnv(logger),
		Queue:                  natsConn,
		EventsBus:              eventsBus,
		PushSubscriptions:      pushSubs,
		NotificationPrefs:      notifPrefs,
		NotificationSent:       notifSent,
		NotifiableRuns:         notifiableRuns,
		WebPushVAPIDPublicKey:  cfg.WebPush.VAPIDPublicKey,
		WebPushVAPIDPrivateKey: cfg.WebPush.VAPIDPrivateKey,
		WebPushSubscriber:      cfg.WebPush.Subscriber,
		MemoryStore:            stores.memory,
		RunSecrets:             stores.runSecrets,
		Sealer:                 sealer,
		OAuthForfait:           stores.oauth,
		OAuthPending:           stores.oauthPending,
		AnthropicOAuthClientID: cfg.Auth.OAuthForfait.AnthropicClientID,
		CodexOAuthClientID:     cfg.Auth.OAuthForfait.CodexClientID,
		AccessTTL:              cfg.Auth.AccessTTL,
		RefreshTTL:             cfg.Auth.RefreshTTL,
		PublicURL:              cfg.Auth.PublicURL,
		SignupMode:             cfg.Auth.SignupMode,
		CookieDomain:           cfg.Auth.CookieDomain,
		CookieSecure:           cfg.Auth.CookieSecure,
		DisableAuth:            disableAuth,
		Metrics:                mreg,
		// /readyz pings each dependency under a 1s deadline. Only Mongo is
		// CRITICAL (it is the store — without it the pod serves nothing
		// real): the others are reported as "degraded" in the probe body
		// and left at 200. Every replica pings the same backends, so
		// gating readiness on all of them turns a 15s NATS/S3 blip into a
		// fleet-wide outage — all pods leave the Service at once and the
		// ingress 503s even the routes that never touch them.
		ReadinessChecks: map[string]server.ReadinessCheck{
			"mongo": {Ping: st.Ping, Critical: true},
			"nats":  {Ping: natsConn.Ping},
			"s3":    {Ping: bc.Ping},
			"valkey": {Ping: func(ctx context.Context) error {
				if redisClient == nil {
					return nil // not configured → not a dependency
				}
				return redisClient.Ping(ctx)
			}},
		},
		// Lame-duck window: /readyz answers 503 for this long before the
		// listener closes, so the endpoints controller can pull the pod
		// out of the Service first (no connection-refused on a deploy or
		// an HPA scale-down).
		ShutdownDelay: cfg.Server.ShutdownDelay,
	}, logger)

	return runServerLoop(rootCtx, srv, mreg, cfg.Metrics.Port, cfg.Server.ShutdownTeardown, logger)
}

// cloudStores bundles every Mongo-backed store the cloud server wires
// into its single server.Config{} literal. Holding them as one value
// keeps runServer's body readable: the construction + schema-ensure
// loop lives in buildCloudStores, and the caller just feeds the
// resulting fields back into the Config.
type cloudStores struct {
	apiKeys          *secrets.MongoApiKeyStore
	genericSecrets   *secrets.MongoGenericSecretStore
	runSecrets       *secrets.MongoRunSecretsStore
	oauth            *secrets.MongoOAuthStore
	oauthPending     *secrets.MongoOAuthPendingStore
	botBindings      *secrets.MongoBotSecretBindingStore
	webhooks         *webhooks.MongoStores
	configShares     *configshare.MongoStore
	forgeConn        *forge.MongoConnectionStore
	forgeIntegration *forge.MongoRepoIntegrationStore
	forgeOAuthApp    *forge.MongoOAuthAppStore
	pluginSources    *pluginsource.MongoStore
	botSources       *botsource.MongoStore
	orgSSO           *orgsso.MongoStore
	orgDomain        *orgsso.MongoDomainStore
	oidcState        *oidc.MongoStateStore
	desktopTickets   *desktopsso.MongoStore
	wsTickets        *wsticket.MongoStore
	orgUsage         *orgusage.MongoCounter
	credPools        *credpool.MongoPoolStore
	credPledges      *credpool.MongoPledgeStore
	credLeases       *credpool.MongoLeaseStore
	credLedger       *credpool.MongoLedger
	audit            *audit.MongoStore
	usageCapSettings *usagecap.MongoSettingsStore
	botRoles         *platformcfg.MongoStore[platformcfg.BotRoles]
	sandboxCfg       *platformcfg.MongoStore[platformcfg.Sandbox]
	marketplace      marketplace.Store
	pat              *pat.MongoStore
	memory           *mongostore.MongoMemoryStore
}

// buildCloudStores constructs every Mongo-backed store the cloud
// server depends on and runs EnsureSchema on them in a single
// table-driven loop. Construction order is preserved exactly so the
// resulting EnsureSchema sequence matches the historical inline
// blocks (api_keys → generic_secrets → … → pat → memory). Marketplace
// is opt-in via ITERION_CLOUD_MARKETPLACE and is appended to the
// table only when enabled.
func buildCloudStores(ctx context.Context, st *mongostore.Store, logger *iterlog.Logger) (*cloudStores, error) {
	s := &cloudStores{
		apiKeys:          secrets.NewMongoApiKeyStore(st.DB()),
		genericSecrets:   secrets.NewMongoGenericSecretStore(st.DB()),
		runSecrets:       secrets.NewMongoRunSecretsStore(st.DB()),
		oauth:            secrets.NewMongoOAuthStore(st.DB()),
		oauthPending:     secrets.NewMongoOAuthPendingStore(st.DB()),
		botBindings:      secrets.NewMongoBotSecretBindingStore(st.DB()),
		webhooks:         webhooks.NewMongoStores(st.DB()),
		configShares:     configshare.NewMongoStore(st.DB()),
		forgeConn:        forge.NewMongoConnectionStore(st.DB()),
		forgeIntegration: forge.NewMongoRepoIntegrationStore(st.DB()),
		forgeOAuthApp:    forge.NewMongoOAuthAppStore(st.DB()),
		pluginSources:    pluginsource.NewMongoStore(st.DB()),
		botSources:       botsource.NewMongoStore(st.DB()),
		botRoles:         platformcfg.NewMongoBotRoles(st.DB()),
		sandboxCfg:       platformcfg.NewMongoSandbox(st.DB()),
		orgSSO:           orgsso.NewMongoStore(st.DB()),
		orgDomain:        orgsso.NewMongoDomainStore(st.DB()),
		// Mongo-backed OIDC state store: PendingAuth must survive across replicas
		// (an OIDC /start on pod A and /callback on pod B), which the per-process
		// memory store can't guarantee in HA.
		oidcState:        oidc.NewMongoStateStore(st.DB(), 10*time.Minute),
		desktopTickets:   desktopsso.NewMongoStore(st.DB(), 2*time.Minute),
		wsTickets:        wsticket.NewMongoStore(st.DB(), time.Minute),
		orgUsage:         orgusage.NewMongoCounter(st.DB()),
		credPools:        credpool.NewMongoPoolStore(st.DB()),
		credPledges:      credpool.NewMongoPledgeStore(st.DB()),
		credLeases:       credpool.NewMongoLeaseStore(st.DB()),
		credLedger:       credpool.NewMongoLedger(st.DB()).WithLogger(logger),
		audit:            audit.NewMongoStore(st.DB()),
		usageCapSettings: usagecap.NewMongoSettingsStore(st.DB()),
		pat:              pat.NewMongoStore(st.DB()),
		memory:           mongostore.NewMongoMemoryStore(st.DB()).WithLogger(logger),
	}

	// Hosted marketplace (Mongo-backed) — opt-in for cloud via
	// ITERION_CLOUD_MARKETPLACE because the submit/install paths are
	// local-mode only today (cloud is rejected pending a vetted submission
	// flow — see pkg/server/marketplace_routes.go). When enabled it surfaces
	// the read-only browse view + sets marketplace_enabled. Local self-host
	// wires a JSONStore unconditionally in pkg/cli/studio.go.
	marketplaceEnabled, _ := strconv.ParseBool(os.Getenv("ITERION_CLOUD_MARKETPLACE"))

	schemas := []schemaEnsurer{
		{"api_keys", s.apiKeys.EnsureSchema},
		{"generic_secrets", s.genericSecrets.EnsureSchema},
		{"run_secrets", s.runSecrets.EnsureSchema},
		{"oauth", s.oauth.EnsureSchema},
		{"oauth_pending", s.oauthPending.EnsureSchema},
		{"bot_secret_bindings", s.botBindings.EnsureSchema},
		{"webhooks", func(c context.Context) error { return webhooks.EnsureSchema(c, st.DB()) }},
		{"forge_connections", s.forgeConn.EnsureSchema},
		{"repo_integrations", s.forgeIntegration.EnsureSchema},
		{"forge_oauth_apps", s.forgeOAuthApp.EnsureSchema},
		{"plugin_sources", s.pluginSources.EnsureSchema},
		{"bot_sources", s.botSources.EnsureSchema},
		{"org_sso_providers", s.orgSSO.EnsureSchema},
		{"org_verified_domains", s.orgDomain.EnsureSchema},
		{"oidc_states", s.oidcState.EnsureSchema},
		{"desktop_sso_tickets", s.desktopTickets.EnsureSchema},
		{"ws_tickets", s.wsTickets.EnsureSchema},
		{"org_usage", func(c context.Context) error { return orgusage.EnsureSchema(c, st.DB()) }},
		{"cred_pool", func(c context.Context) error { return credpool.EnsureSchema(c, st.DB()) }},
		{"audit", func(c context.Context) error { return audit.EnsureSchema(c, st.DB()) }},
		{"board", func(c context.Context) error { return boardmongo.EnsureSchema(c, st.DB()) }},
		{"trigger_subscriptions", func(c context.Context) error { return trigger.NewMongoSubscriptionStore(st.DB()).EnsureSchema(c) }},
		{"scheduled_bots", func(c context.Context) error { return cloudsched.EnsureSchema(c, st.DB()) }},
		{"config_shares", func(c context.Context) error { return configshare.EnsureSchema(c, st.DB()) }},
	}
	if marketplaceEnabled {
		schemas = append(schemas, schemaEnsurer{"marketplace", func(c context.Context) error { return marketplace.EnsureSchema(c, st.DB()) }})
	}
	schemas = append(schemas,
		schemaEnsurer{"pat", func(c context.Context) error { return pat.EnsureSchema(c, st.DB()) }},
		schemaEnsurer{"memory", s.memory.EnsureSchema},
	)

	if err := runSchemaEnsurers(ctx, schemas); err != nil {
		return nil, err
	}
	if marketplaceEnabled {
		s.marketplace = marketplace.NewMongoStore(st.DB())
	}
	return s, nil
}

// newPluginSourceResolver builds the launch-time resolver for team-scoped,
// git-hosted plugins (ADR-080). The cache dir is deliberately ephemeral: the
// durable authority is the Mongo record, so a cold pod re-derives its checkouts
// instead of depending on pod-local state that a restart would silently lose.
//
// The read credential is used strictly BY REFERENCE — the secret id travels on
// the source record, the value is unsealed here and handed to the fetcher,
// which passes it to git via an askpass helper (never argv, never a log line).
func newPluginSourceResolver(stores *cloudStores, sealer secrets.Sealer, logger *iterlog.Logger) *pluginsource.Resolver {
	if stores == nil || stores.pluginSources == nil || sealer == nil {
		return nil
	}
	return &pluginsource.Resolver{
		Store: stores.pluginSources,
		Fetcher: &pluginsource.Fetcher{
			CacheDir: filepath.Join(os.TempDir(), "iterion-plugin-sources"),
			CredentialFor: func(ctx context.Context, s pluginsource.PluginSource) (string, error) {
				if s.SecretID == "" {
					return "", nil // public repository
				}
				gs, err := stores.genericSecrets.Get(store.WithTenant(ctx, s.TenantID), s.SecretID)
				if err != nil {
					return "", err
				}
				plain, err := secrets.OpenGenericSecret(sealer, gs.ID, gs.SealedSecret)
				if err != nil {
					return "", err
				}
				return string(plain), nil
			},
		},
		Warnf: logger.Warn,
	}
}

// schemaEnsurer pairs a Mongo collection label (used in the
// "server: ensure <label> schema" error wrapping) with the
// EnsureSchema callable. buildCloudStores + buildAuthStack walk a
// slice of these in a single loop so each schema-ensure line stays a
// one-liner.
type schemaEnsurer struct {
	label string
	fn    func(context.Context) error
}

// runSchemaEnsurers walks a slice of schemaEnsurer entries, applying
// the standard "server: ensure <label> schema: %w" error wrapping on
// the first failure. Stops on the first error.
func runSchemaEnsurers(ctx context.Context, ensurers []schemaEnsurer) error {
	for _, e := range ensurers {
		if err := e.fn(ctx); err != nil {
			return fmt.Errorf("server: ensure %s schema: %w", e.label, err)
		}
	}
	return nil
}

// authStack holds the auth-side dependencies built after the cloud
// stores: the identity/session stores, the JWT signer, and the
// assembled auth.Service. Returned as a bundle so runServer can feed
// individual fields back into server.Config without juggling a long
// argument list.
type authStack struct {
	identityStore *identity.MongoStore
	sessions      *iterauth.MongoSessionStore
	signer        *iterauth.JWTSigner
	resetStore    *iterauth.MongoPasswordResetStore
	authSvc       *iterauth.Service
}

// buildAuthStack wires the identity store, sessions store, JWT
// signer, mailer, password-reset store, and the assembled
// auth.Service. The construction + schema-ensure order matches the
// historical inline sequence (identity → sessions → password_resets).
// Mailer construction is delegated to buildMailer (SMTP when
// ITERION_SMTP_HOST is set, else the log fallback).
func buildAuthStack(ctx context.Context, cfg iterconfig.Config, st *mongostore.Store, stores *cloudStores, logger *iterlog.Logger) (*authStack, error) {
	a := &authStack{
		identityStore: identity.NewMongoStore(st.DB()),
		sessions:      iterauth.NewMongoSessionStore(st.DB()),
		resetStore:    iterauth.NewMongoPasswordResetStore(st.DB()),
	}

	if err := runSchemaEnsurers(ctx, []schemaEnsurer{
		{"identity", a.identityStore.EnsureSchema},
		{"sessions", a.sessions.EnsureSchema},
	}); err != nil {
		return nil, err
	}

	signer, err := iterauth.NewJWTSigner(cfg.Auth.JWTSecret, cfg.Auth.AccessTTL)
	if err != nil {
		return nil, fmt.Errorf("server: build jwt signer: %w", err)
	}
	a.signer = signer

	// SMTP: ITERION_SMTP_HOST switches the real mailer on; otherwise the log
	// fallback keeps flows testable and server_info reports email_enabled=false
	// so the SPA hides forgot-password.
	mailer, err := buildMailer(logger)
	if err != nil {
		return nil, err
	}
	if err := runSchemaEnsurers(ctx, []schemaEnsurer{
		{"password_resets", a.resetStore.EnsureSchema},
	}); err != nil {
		return nil, err
	}

	svc, err := iterauth.NewService(iterauth.Config{
		Store:                    a.identityStore,
		Sessions:                 a.sessions,
		Signer:                   signer,
		SignupMode:               iterauth.SignupMode(cfg.Auth.SignupMode),
		GitHubUngrantedPolicy:    iterauth.GitHubUngrantedPolicy(cfg.Auth.OIDC.GitHubUngrantedPolicy),
		RefreshTTL:               cfg.Auth.RefreshTTL,
		Logger:                   logger,
		Resets:                   a.resetStore,
		Mailer:                   mailer,
		PublicURL:                cfg.Auth.PublicURL,
		OrgSSO:                   stores.orgSSO,
		Domains:                  stores.orgDomain,
		TrustedAutoLinkProviders: cfg.Auth.TrustedAutoLinkProviders,
	})
	if err != nil {
		return nil, fmt.Errorf("server: build auth service: %w", err)
	}
	a.authSvc = svc
	return a, nil
}

// patMaxTTLFromEnv parses ITERION_PAT_MAX_TTL (a Go duration, e.g.
// "2160h" = 90 days) into the platform-wide cap applied to every
// personal access token. Unset or invalid values return 0 (no cap);
// an invalid value is logged so the operator notices.
func patMaxTTLFromEnv(logger *iterlog.Logger) time.Duration {
	v := os.Getenv("ITERION_PAT_MAX_TTL")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logger.Warn("server: invalid ITERION_PAT_MAX_TTL %q ignored", v)
		return 0
	}
	return d
}

// botsPathsFromEnv reads ITERION_BOTS_PATH — the colon-separated list
// of directories the inbound-webhook bot resolver searches for
// recipes. The official image ships the catalog at /opt/iterion/bots
// and sets this; an empty value means studio still discovers bots via
// WorkDir but webhook-driven launches won't find anything.
func botsPathsFromEnv() []string {
	bp := os.Getenv("ITERION_BOTS_PATH")
	if bp == "" {
		return nil
	}
	return filepath.SplitList(bp)
}

// runServerLoop starts the dedicated Prometheus metrics listener and
// the main HTTP server, then blocks until SIGINT/SIGTERM (via the
// parent rootCtx) or the server returns an error. On signal it runs a
// graceful shutdown bounded by teardown; on listen error it propagates
// the error upstream. Metrics startup is synchronous so a port-conflict
// surfaces at boot, not later.
func runServerLoop(rootCtx context.Context, srv *server.Server, mreg *metrics.Registry, metricsPort int, teardown time.Duration, logger *iterlog.Logger) error {
	// Prometheus metrics on a dedicated port (plan §F T-40). Bound
	// synchronously so a port-conflict surfaces at boot, not later.
	metricsAddr := fmt.Sprintf(":%d", metricsPort)
	metricsSrv, err := mreg.StartServer(metricsAddr, logger)
	if err != nil {
		return fmt.Errorf("server: start metrics: %w", err)
	}
	defer func() { _ = metrics.ShutdownServer(metricsSrv) }()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-rootCtx.Done():
		// The teardown budget sits ON TOP of the lame-duck delay the
		// server waits out first — otherwise the delay would eat the
		// window in-flight requests need. k8s
		// terminationGracePeriodSeconds must cover the sum (the chart
		// comments the arithmetic).
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), srv.ShutdownDelay()+teardown)
		defer shutdownCancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// bootstrapAdmin reconciles the configured super-admin per the
// ITERION_BOOTSTRAP_ADMIN_* policy. No-op when no email is configured or auth
// is disabled. A declared password (ITERION_BOOTSTRAP_ADMIN_PASSWORD, a k8s
// secret) is AUTHORITATIVE and reconciled idempotently; an un-activated admin
// gets a fresh temp password re-issued; an empty users collection gets the
// admin created. Passwords are never logged on the declarative path.
func bootstrapAdmin(ctx context.Context, cfg iterconfig.Config, identityStore *identity.MongoStore, authSvc *iterauth.Service, disableAuth bool, logger *iterlog.Logger) error {
	email := cfg.Auth.BootstrapAdminEmail
	if email == "" || disableAuth {
		return nil
	}
	declaredPW := strings.TrimSpace(cfg.Auth.BootstrapAdminPassword)
	existing, getErr := identityStore.GetUserByEmail(ctx, email)
	switch {
	case getErr == nil:
		switch {
		case declaredPW != "":
			// Declarative admin: the secret is AUTHORITATIVE — ensure an active
			// super-admin whose password matches it, resetting only on drift so
			// idempotent restarts are no-ops. The password is never logged.
			changed := false
			if !existing.IsSuperAdmin {
				existing.IsSuperAdmin = true
				changed = true
			}
			if existing.Status != identity.UserStatusActive {
				existing.Status = identity.UserStatusActive
				changed = true
			}
			if ok, _ := iterauth.VerifyPassword(declaredPW, existing.PasswordHash); !ok {
				hash, err := iterauth.HashPassword(declaredPW)
				if err != nil {
					return fmt.Errorf("server: hash bootstrap password: %w", err)
				}
				existing.PasswordHash = hash
				changed = true
				logger.Info("server: BOOTSTRAP super-admin %s password reconciled to ITERION_BOOTSTRAP_ADMIN_PASSWORD", email)
			}
			if changed {
				if err := identityStore.UpdateUser(ctx, existing); err != nil {
					return fmt.Errorf("server: reconcile bootstrap admin: %w", err)
				}
			}
		case existing.Status == identity.UserStatusPendingPasswordChange:
			// No declared password and the admin was never activated. Re-issue
			// a fresh temp password so an operator who lost the first one (e.g.
			// the pod restarted before it was captured) can recover by
			// restarting. An already-active admin's password is never reset.
			pw, err := randomBootstrapPassword()
			if err != nil {
				return fmt.Errorf("server: bootstrap password: %w", err)
			}
			hash, err := iterauth.HashPassword(pw)
			if err != nil {
				return fmt.Errorf("server: hash bootstrap password: %w", err)
			}
			existing.PasswordHash = hash
			if err := identityStore.UpdateUser(ctx, existing); err != nil {
				return fmt.Errorf("server: re-issue bootstrap admin: %w", err)
			}
			logger.Warn("server: BOOTSTRAP super-admin %s still pending — re-issued temp_password=%s (rotate via POST /api/auth/password/change, or set ITERION_BOOTSTRAP_ADMIN_PASSWORD)", email, pw)
		}
		// getErr == nil && active && no declared password → no-op.
	case errors.Is(getErr, identity.ErrNotFound):
		// First boot with an empty users collection → create the admin.
		count, err := identityStore.UserCount(ctx)
		if err != nil {
			return fmt.Errorf("server: user count: %w", err)
		}
		if count == 0 {
			if declaredPW != "" {
				// Declarative: create an ACTIVE super-admin with the secret
				// password — no temp-password dance, GitOps-friendly.
				if _, _, err := authSvc.CreateUserAndPersonalTeam(ctx, email, "Bootstrap admin", declaredPW, true, identity.UserStatusActive); err != nil {
					return fmt.Errorf("server: bootstrap admin: %w", err)
				}
				logger.Info("server: BOOTSTRAP super-admin created (active) from ITERION_BOOTSTRAP_ADMIN_PASSWORD — email=%s", email)
			} else {
				pw, err := randomBootstrapPassword()
				if err != nil {
					return fmt.Errorf("server: bootstrap password: %w", err)
				}
				if _, _, err := authSvc.CreateUserAndPersonalTeam(ctx, email, "Bootstrap admin", pw, true, identity.UserStatusPendingPasswordChange); err != nil {
					return fmt.Errorf("server: bootstrap admin: %w", err)
				}
				logger.Warn("server: BOOTSTRAP super-admin created — email=%s temp_password=%s (rotate via POST /api/auth/password/change, or set ITERION_BOOTSTRAP_ADMIN_PASSWORD)", email, pw)
			}
		}
	case getErr != nil:
		return fmt.Errorf("server: bootstrap admin lookup: %w", getErr)
	}
	return nil
}

// buildMailer returns an SMTP mailer when ITERION_SMTP_HOST is set, else a
// log-only fallback (which keeps auth flows testable and reports
// email_enabled=false to the SPA).
func buildMailer(logger *iterlog.Logger) (mail.Mailer, error) {
	host := os.Getenv("ITERION_SMTP_HOST")
	if host == "" {
		return &mail.LogMailer{Logger: logger}, nil
	}
	port, _ := strconv.Atoi(os.Getenv("ITERION_SMTP_PORT"))
	startTLS := true
	if v := os.Getenv("ITERION_SMTP_STARTTLS"); v != "" {
		startTLS, _ = strconv.ParseBool(v)
	}
	smtpMailer, err := mail.NewSMTP(mail.Config{
		Host:     host,
		Port:     port,
		Username: os.Getenv("ITERION_SMTP_USERNAME"),
		Password: os.Getenv("ITERION_SMTP_PASSWORD"),
		From:     os.Getenv("ITERION_SMTP_FROM"),
		StartTLS: startTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("server: smtp config: %w", err)
	}
	logger.Info("server: SMTP mailer enabled (host=%s)", host)
	return smtpMailer, nil
}

// buildOIDCRegistry wires the enabled OIDC connectors (Google, GitHub,
// generic) into a fresh registry.
func buildOIDCRegistry(cfg iterconfig.Config) *oidc.Registry {
	registry := oidc.NewRegistry()
	if cfg.Auth.OIDC.Google.Enabled {
		registry.Register(oidc.NewGoogleConnector(cfg.Auth.OIDC.Google.ClientID, cfg.Auth.OIDC.Google.ClientSecret, cfg.Auth.OIDC.Google.DisplayName))
	}
	if cfg.Auth.OIDC.GitHub.Enabled {
		registry.Register(oidc.NewGitHubConnector(cfg.Auth.OIDC.GitHub.ClientID, cfg.Auth.OIDC.GitHub.ClientSecret, cfg.Auth.OIDC.GitHub.DisplayName))
	}
	if cfg.Auth.OIDC.Generic.Enabled {
		registry.Register(oidc.NewGenericConnector(cfg.Auth.OIDC.Generic.IssuerURL, cfg.Auth.OIDC.Generic.ClientID, cfg.Auth.OIDC.Generic.ClientSecret, cfg.Auth.OIDC.Generic.DisplayName, cfg.Auth.OIDC.Generic.Scopes))
	}
	return registry
}

// platformSandboxImageResolver builds the publish-time resolver for the
// platform sandbox default-image setting: a TTL-cached read of the
// `sandbox` settings family, pinned onto each RunMessage so a redelivery
// reruns in the same environment. Returns "" (inherit env/built-in) when
// no override is stored.
func platformSandboxImageResolver(stores *cloudStores, logger *iterlog.Logger) func(context.Context) string {
	resolver := platformcfg.NewResolver[platformcfg.Sandbox](stores.sandboxCfg, logger.Warn)
	return func(ctx context.Context) string {
		rec := resolver.Get(ctx)
		if rec == nil || rec.DefaultImage == nil {
			return ""
		}
		return strings.TrimSpace(*rec.DefaultImage)
	}
}
