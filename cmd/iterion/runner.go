package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/cloud/tracing"
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runner"
	k8ssandbox "github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/secrets"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/usagecap"
	"github.com/spf13/cobra"
)

// resolveRunnerSandboxDefault applies sandbox-by-default to the runner
// entry point: an explicit config value (ITERION_SANDBOX_DEFAULT or the
// config file) wins; unset resolves to auto, mirroring
// runtime.ResolveGlobalSandboxDefault for the other entry points. Set
// ITERION_SANDBOX_DEFAULT=none to restore the historical behaviour.
func resolveRunnerSandboxDefault(configured string) string {
	if v := strings.ToLower(strings.TrimSpace(configured)); v != "" {
		return v
	}
	return "auto"
}

var runnerConfigPath string

var runnerCmd = &cobra.Command{
	Use:   "runner",
	Short: "Run an iterion runner pod (cloud-mode workflow executor)",
	Long: `Connect to NATS, claim leases via the run-lock KV bucket, and execute
RunMessages from the iterion.queue.runs JetStream subject. Persists
state to MongoDB and artifact bodies to S3.

Configuration is environment-driven (ITERION_*) per cloud-ready plan
§E. A YAML file passed via --config is merged before env vars (env
wins) so an operator can layer overrides on top of a baseline.`,
	RunE: runRunner,
}

func init() {
	runnerCmd.Flags().StringVar(&runnerConfigPath, "config", "", "path to YAML config (env vars take precedence)")
	rootCmd.AddCommand(runnerCmd)
}

func runRunner(cmd *cobra.Command, _ []string) error {
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{
		YAMLPath:         runnerConfigPath,
		DefaultLogFormat: iterconfig.LogFormatJSON,
	})
	if err != nil {
		return fmt.Errorf("runner: load config: %w", err)
	}
	if cfg.Mode != iterconfig.ModeCloud {
		return fmt.Errorf("runner: ITERION_MODE must be 'cloud' (got %q)", cfg.Mode)
	}

	logger := cfg.Log.NewLogger(cmd.ErrOrStderr())
	errtrack.Init(errtrack.Config{Logger: logger, ServerName: "iterion-runner"})
	errtrack.AttachLogHook(logger)
	defer errtrack.Flush()
	logger.Info("runner: starting")

	rootCtx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	traceShutdown, err := tracing.Init(rootCtx, "iterion-runner", logger)
	if err != nil {
		return fmt.Errorf("runner: init tracing: %w", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = traceShutdown(shutCtx)
	}()

	// 1. NATS layer — provides the queue + KV lock bucket.
	// Keep in sync with the natsq.Connect literal in server.go: a field only
	// one side passes is silently defaulted for the other, and the two then
	// disagree about the same broker. LockTTL is what just drifted.
	natsConn, err := natsq.Connect(rootCtx, natsq.Config{
		URL:                 cfg.NATS.URL,
		StreamName:          cfg.NATS.Stream,
		DLQStream:           cfg.NATS.DLQStream,
		KVBucket:            cfg.NATS.KVBucket,
		StreamReplicas:      cfg.NATS.StreamReplicas,
		MaxAckPending:       cfg.NATS.MaxAckPending,
		AckWait:             cfg.NATS.AckWait,
		SchemaMismatchDelay: cfg.Runner.SchemaMismatchDelay,
		EpochMismatchDelay:  cfg.Rollout.EpochMismatchDelay,
		RunnerEpoch:         cfg.Rollout.RunnerEpoch,
		MaxDeliver:          cfg.NATS.MaxDeliver,
		MaxAge:              cfg.NATS.MaxAge,
		DLQMaxAge:           cfg.NATS.DLQMaxAge,
		MaxPayload:          cfg.NATS.MaxPayload,
		LockTTL:             cfg.Runner.LockTTL,
		Logger:              logger,
	})
	if err != nil {
		return fmt.Errorf("runner: connect NATS: %w", err)
	}
	defer natsConn.Close()

	// 2. Blob (S3 / MinIO) — backs WriteArtifact/LoadArtifact.
	bc, err := newCloudBlob(rootCtx, cfg.S3)
	if err != nil {
		return fmt.Errorf("runner: build blob client: %w", err)
	}
	defer func() { _ = bc.Close() }()

	// 3. Mongo store with NATS-KV-backed lock provider so LockRun
	//    returns a real distributed lease (vs the no-op in
	//    server-side cloud store usage).
	runnerID, _ := os.Hostname()
	lockProv := natsq.NewLockProvider(natsConn, runnerID)
	st, err := newCloudMongoStore(rootCtx, cfg.Mongo, bc, logger, lockProv)
	if err != nil {
		return fmt.Errorf("runner: build mongo store: %w", err)
	}
	defer closeCloudStoreWithTimeout(st)

	// 4. Prometheus metrics + the kubelet probes on a dedicated port.
	//    Bound before the runner is built so a port conflict surfaces at
	//    boot; until runner.New publishes the health source the probes
	//    answer "starting" (503 on /readyz), which is the honest state.
	mreg := metrics.New()
	metricsAddr := fmt.Sprintf(":%d", cfg.Metrics.Port)
	health := &runner.HealthProvider{}
	metricsSrv, err := mreg.StartServer(metricsAddr, logger,
		metrics.Mount{Path: "/healthz", Handler: health.LivenessHandler()},
		metrics.Mount{Path: "/readyz", Handler: health.ReadinessHandler()},
	)
	if err != nil {
		return fmt.Errorf("runner: start metrics: %w", err)
	}
	defer func() { _ = metrics.ShutdownServer(metricsSrv) }()

	selfEpoch, highWaterEpoch := natsConn.RunnerEpoch()
	if natsConn.Superseded() {
		mreg.RolloutEpochRegression.WithLabelValues("runner").Inc()
		health.Set(func() runner.Health {
			return runner.Health{
				Superseded:     true,
				Epoch:          selfEpoch,
				HighWaterEpoch: highWaterEpoch,
			}
		})
		logger.WithFields(map[string]any{"self_epoch": selfEpoch, "high_water_epoch": highWaterEpoch}).Error("runner: epoch regression detected — staying live but non-ready; no queue consumer started")
		<-rootCtx.Done()
		return nil
	}

	// 4b. BYOK / OAuth wire-up. The runner consumes sealed bundles
	//     keyed by RunMessage.SecretsRef; the master key MUST match
	//     the publisher's. Phase C.
	sealer, err := secrets.NewAESGCMSealerFromBase64(cfg.Auth.SecretsKey)
	if err != nil {
		return fmt.Errorf("runner: build sealer: %w", err)
	}
	runSecretsStore := secrets.NewMongoRunSecretsStore(st.DB())
	if err := runSecretsStore.EnsureSchema(rootCtx); err != nil {
		return fmt.Errorf("runner: ensure run_secrets schema: %w", err)
	}
	// Tell pkg/backend/model how to read per-run credentials from
	// ctx. We translate provider names (string) ↔ secrets.Provider
	// enum here so the model package stays free of pkg/secrets imports.
	model.SetCredentialsLookup(func(ctx context.Context) (func(string) string, bool) {
		creds, ok := secrets.CredentialsFromContext(ctx)
		if !ok {
			return nil, false
		}
		return func(provider string) string {
			return creds.APIKey(secrets.Provider(provider))
		}, true
	})
	// Per-run OAuth-forfait dirs (codex / claude_code) the runner materialised
	// at claim time. Lets the in-process claw model factory consume a tenant's
	// resolved subscription in cloud mode, where the pod has neither ~/.codex
	// nor ~/.claude: codex → openai, claude_code → anthropic.
	model.SetOAuthDirLookup(func(ctx context.Context) (func(string) string, bool) {
		creds, ok := secrets.CredentialsFromContext(ctx)
		if !ok {
			return nil, false
		}
		return func(kind string) string {
			return creds.OAuthDir(kind)
		}, true
	})

	// Shared knowledge memory persists in the tenant's document store
	// (not the pod's ephemeral disk) so it survives across runs/pods.
	memStore := mongostore.NewMongoMemoryStore(st.DB()).WithLogger(logger)
	if err := memStore.EnsureSchema(rootCtx); err != nil {
		return fmt.Errorf("runner: ensure memory schema: %w", err)
	}

	// Org metering: the runner charges each run's accumulated LLM
	// cost/tokens to the org's monthly bucket (the same collection the
	// server's launch gate + usage views read).
	orgUsageCounter := orgusage.NewMongoCounter(st.DB())
	// Per-credential metering: the same attempt, read by credential rather
	// than by org. Schema ensured beside orgusage's below.
	credUsageCounter := credusage.NewMongoCounter(st.DB())
	if err := orgusage.EnsureSchema(rootCtx, st.DB()); err != nil {
		return fmt.Errorf("runner: ensure org_usage schema: %w", err)
	}
	if err := credusage.EnsureSchema(rootCtx, st.DB()); err != nil {
		return fmt.Errorf("runner: ensure credential_usage schema: %w", err)
	}

	// Credential pool: a run served by a contributor's lent subscription
	// must report what it spent, or the donor's ledger never moves and
	// their concurrency slot only frees on lease expiry.
	credBroker := credpool.NewBroker(credpool.BrokerConfig{
		Pools:   credpool.NewMongoPoolStore(st.DB()),
		Pledges: credpool.NewMongoPledgeStore(st.DB()),
		Leases:  credpool.NewMongoLeaseStore(st.DB()),
		Ledger:  credpool.NewMongoLedger(st.DB()).WithLogger(logger),
		OAuth:   secrets.NewMongoOAuthStore(st.DB()),
		APIKeys: secrets.NewMongoApiKeyStore(st.DB()),
		Sealer:  sealer,
		Logger:  logger,
	})

	// Usage cap: the operator's ceiling on the LLM subscription's own
	// five-hour / weekly windows. A malformed policy stops the runner
	// here — every wrong answer downstream fails open, and a fleet that
	// silently stopped honouring its cap is what this guards against.
	// The env values are the DEFAULTS; the platform runtime-settings
	// record (mutable through the server's admin API) overrides the
	// percentages live, TTL-cached, so both deployments enforce the same
	// number without a restart.
	usageCapEnvPolicy, err := usagecap.FromEnv()
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	usageCapTrust, err := usagecap.TrustFromEnv()
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	usageCapStore := usagecap.NewMongoStore(st.DB())
	usageCapSource := usagecap.NewResolver(
		usagecap.NewMongoSettingsStore(st.DB()), usageCapEnvPolicy,
		usagecap.WithWarnLogger(logger.Warn))
	// Bot-var settings (platformcfg FamilyBotVars) reach every
	// ${ITERION_X:-default} expansion the runner performs — model pins,
	// reasoning effort, tunables — through the ir overlay. Installed once
	// at boot; each claimed run sees the values within the resolver TTL,
	// no restart. Precedence: setting > pod env > .bot default.
	botVarsResolver := platformcfg.NewResolver[platformcfg.BotVars](
		platformcfg.NewMongoBotVars(st.DB()), logger.Warn)
	ir.SetEnvOverlay(func(name string) (string, bool) {
		rec := botVarsResolver.Get(context.Background())
		if rec == nil {
			return "", false
		}
		v, ok := rec.Vars[name]
		return v, ok
	})
	// The schema is ensured unconditionally: a cap disabled in env can be
	// armed at runtime through the settings record, and the readings
	// ledger must exist by then.
	if err := usagecap.EnsureSchema(rootCtx, st.DB()); err != nil {
		return fmt.Errorf("runner: ensure usage_windows schema: %w", err)
	}
	if effective := usageCapSource.Effective(rootCtx); effective.Enabled() {
		logger.Info("runner: usage cap armed — %s", effective)
	}

	// Bots: where bot-qualified runs resolve their bundle so skills/
	// mirror into the workspace — same env contract as the server
	// (the official image sets ITERION_BOTS_PATH=/opt/iterion/bots).
	var botsPaths []string
	if bp := os.Getenv("ITERION_BOTS_PATH"); bp != "" {
		botsPaths = filepath.SplitList(bp)
	}

	// Event spine: publish run outcomes (finished/failed/cancelled/paused)
	// onto the NATS event subjects so server-side consumers (user
	// notifications, trigger chaining) see runner-pod runs. Rides the
	// queue's connection — disjoint subject trees on one link.
	eventsBus, err := eventbus.NewNATSBus(natsConn.NATS(), eventbus.NATSOptions{Logger: logger})
	if err != nil {
		return fmt.Errorf("runner: build events bus: %w", err)
	}
	// A malformed sandbox scheduling policy must stop the rollout here, before
	// the consumer exists and the epoch mark advances: the sandbox driver
	// factory skips constructor errors, so nothing downstream can refuse it —
	// every run would fail at sandbox start instead.
	if err := k8ssandbox.ValidateSchedulingEnv(); err != nil {
		return fmt.Errorf("runner: sandbox scheduling policy: %w", err)
	}

	// Prove the durable consumer can be created before advancing the rollout
	// high-water mark. The handle is inert until Runner.Run starts fetching.
	preparedConsumer, err := natsConn.PrepareConsumer(rootCtx)
	if err != nil {
		return fmt.Errorf("runner: prepare queue consumer: %w", err)
	}

	// 5. Runner loop.
	r, err := runner.New(rootCtx, runner.Config{
		NATS:                natsConn,
		PreparedConsumer:    preparedConsumer,
		Events:              eventsBus,
		Store:               st,
		RunnerID:            runnerID,
		WorkDir:             cfg.Runner.WorkDir,
		HeartbeatInterval:   cfg.Runner.Heartbeat,
		DrainMode:           cfg.Runner.DrainMode,
		DrainTimeout:        cfg.Runner.DrainTimeout,
		SchemaMismatchDelay: cfg.Runner.SchemaMismatchDelay,
		RunnerEpoch:         selfEpoch,
		HighWaterEpoch:      highWaterEpoch,
		EpochMismatchDelay:  cfg.Rollout.EpochMismatchDelay,
		Logger:              logger,
		Metrics:             mreg,
		RunSecrets:          runSecretsStore,
		Sealer:              sealer,
		GenericSecrets:      secrets.NewMongoGenericSecretStore(st.DB()),
		// BYOK store shared with the publisher — the runner bumps
		// `last_used_at` at metering time so the studio distinguishes an
		// idle key from one currently serving (#659 pt 2).
		ApiKeys:        secrets.NewMongoApiKeyStore(st.DB()),
		MemoryStore:    memStore,
		OrgUsage:       orgUsageCounter,
		CredUsage:      credUsageCounter,
		CredPool:       credBroker,
		UsageCapSource: usageCapSource,
		UsageCaps:      usageCapStore,
		UsageCapTrust:  usageCapTrust,
		BotsPaths:      botsPaths,
		BotSources:     botsource.NewMongoStore(st.DB()),
		// Sandbox-by-default: the runner is a product entry point like
		// `iterion run` — an unset ITERION_SANDBOX_DEFAULT resolves to
		// auto. Discovered live (run 019f8a05): lifting the chart's
		// ITERION_SANDBOX_OVERRIDE=none without this left cloud runs
		// executing unsandboxed in the runner pod, because the runner
		// wired the raw (empty) config value and the engine is neutral.
		SandboxDefault:   resolveRunnerSandboxDefault(cfg.Sandbox.Default),
		SandboxHostState: cfg.Sandbox.HostState,
		SandboxOverride:  cfg.Sandbox.Override,
	})
	if err != nil {
		return fmt.Errorf("runner: build: %w", err)
	}

	// Claim only after every fallible dependency and the inert consumer have
	// been wired. A broken epoch-bump release therefore cannot poison the
	// durable mark and fence the still-healthy previous generation.
	if err := natsConn.ClaimRunnerEpoch(rootCtx); err != nil {
		return fmt.Errorf("runner: claim rollout epoch: %w", err)
	}
	selfEpoch, highWaterEpoch = natsConn.RunnerEpoch()
	r.SetRolloutState(selfEpoch, highWaterEpoch, natsConn.Superseded())
	health.Set(r.Health)
	if natsConn.Superseded() {
		mreg.RolloutEpochRegression.WithLabelValues("runner").Inc()
		logger.WithFields(map[string]any{"self_epoch": selfEpoch, "high_water_epoch": highWaterEpoch}).Error("runner: epoch superseded while bootstrapping — staying live but non-ready; no queue consumer started")
		<-rootCtx.Done()
		return nil
	}

	// SIGTERM handling: stop fetching, then drain per DrainMode — lame-duck
	// (let the in-flight run finish) or interrupt (cancel + checkpoint for
	// auto-resume). The drain ceiling is DrainTimeout; k8s
	// terminationGracePeriodSeconds is the hard external bound (must be >=
	// DrainTimeout + margin so a capped run checkpoints cleanly).
	go func() {
		// A panic here would take the pod down on its way out with no
		// trace: main's recover only covers its own goroutine. Capture,
		// then let it die exactly as it did.
		defer func() {
			if p := recover(); p != nil {
				errtrack.CapturePanicFields(p, map[string]any{"surface": "runner.drain"})
				panic(p)
			}
		}()
		<-rootCtx.Done()
		logger.Info("runner: shutdown signal received (drain mode=%s ceiling=%s)", r.DrainMode(), r.DrainTimeout())
		drainCtx, drainCancel := context.WithTimeout(context.Background(), r.DrainTimeout())
		defer drainCancel()
		_ = r.Shutdown(drainCtx)
	}()

	if err := r.Run(rootCtx); err != nil && err != context.Canceled {
		return fmt.Errorf("runner: loop: %w", err)
	}
	logger.Info("runner: exited cleanly")
	return nil
}
