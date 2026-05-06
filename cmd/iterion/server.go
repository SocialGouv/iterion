package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/eventstream"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// `iterion server` is the cloud-mode HTTP server entry point. In
// local mode it delegates to cli.RunEditor (same handler tree as
// `iterion editor`). In cloud mode it builds a Mongo+S3 store + a
// NATS-backed LaunchPublisher and feeds them into pkg/server.Server
// so handleLaunchRun publishes to the queue instead of spawning the
// runtime in-process.
//
// Differences from `iterion editor` regardless of mode:
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
	Short: "Start the iterion HTTP server (editor SPA + run console + cloud API)",
	Long: `iterion server is the cloud-deployment HTTP entry point. It serves the
editor SPA, the run console (REST + WebSocket), and the launch /
resume / cancel API on a single port. Health endpoints (/healthz,
/readyz) live alongside the API.

Mode is chosen by ITERION_MODE:
  - local (default): in-process engine; same as 'iterion editor'.
  - cloud: persists to Mongo+S3, publishes runs onto NATS for the
    runner pool to consume.

For local dev, prefer 'iterion editor' which keeps the loopback bind
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
	rootCmd.AddCommand(serverCmd)
}

func runServer(cmd *cobra.Command, _ []string) error {
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{
		YAMLPath:         serverOpts.configPath,
		DefaultLogFormat: iterconfig.LogFormatJSON,
	})
	if err != nil {
		return fmt.Errorf("server: load config: %w", err)
	}

	// Local mode: keep the existing editor handlers; only difference
	// from `iterion editor` is the cloud-friendly --bind default.
	if cfg.Mode == iterconfig.ModeLocal {
		return cli.RunEditor(cmd.Context(), cli.EditorOptions{
			Port:      serverOpts.port,
			Bind:      serverOpts.bind,
			Dir:       serverOpts.dir,
			StoreDir:  serverOpts.storeDir,
			NoBrowser: true,
		}, newPrinter())
	}

	// Cloud mode: build Mongo+S3 store + NATS publisher + server
	// directly. We bypass cli.RunEditor because it auto-discovers a
	// filesystem store, which doesn't make sense when persistence
	// lives in Mongo.
	logger := iterlog.NewWithFormat(parseLevel(cfg.Log.Level), cmd.ErrOrStderr(), parseLogFormat(cfg.Log.Format))
	logger.Info("server: starting (mode=cloud)")

	rootCtx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	natsConn, err := natsq.Connect(rootCtx, natsq.Config{
		URL:        cfg.NATS.URL,
		StreamName: cfg.NATS.Stream,
		DLQStream:  cfg.NATS.DLQStream,
		KVBucket:   cfg.NATS.KVBucket,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("server: connect NATS: %w", err)
	}
	defer natsConn.Close()

	bc, err := blob.NewS3(rootCtx, blob.Config{
		Endpoint:        cfg.S3.Endpoint,
		Region:          cfg.S3.Region,
		Bucket:          cfg.S3.Bucket,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		UsePathStyle:    cfg.S3.UsePathStyle,
	})
	if err != nil {
		return fmt.Errorf("server: build blob client: %w", err)
	}

	// Server-side store: no NATS lock provider — the server never
	// executes runs, only publishes them. The runner pod is the
	// only place that takes leases.
	st, err := mongostore.New(rootCtx, mongostore.Config{
		URI:           cfg.Mongo.URI,
		Database:      cfg.Mongo.DB,
		EventsTTLDays: cfg.Mongo.EventsTTLDays,
		Logger:        logger,
		Blob:          bc,
	})
	if err != nil {
		return fmt.Errorf("server: build mongo store: %w", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = st.Close(closeCtx)
	}()

	pub, err := cloudpublisher.New(cloudpublisher.Config{
		NATS:      natsConn,
		Store:     st,
		MongoColl: st.RunsCollection(),
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("server: build cloud publisher: %w", err)
	}

	// Mongo change-stream event source so the WS handler streams
	// runner-pod events (the local broker would only see this
	// process's writes). Plan §F (T-21, T-22).
	mongoSource := eventstream.NewMongo(st.EventsCollection(), logger)
	eventSrc := runview.NewEventSourceAdapter(mongoSource)

	if cfg.Server.SessionToken == "" {
		disableAuth, _ := strconv.ParseBool(os.Getenv("ITERION_DISABLE_AUTH"))
		if !disableAuth {
			return fmt.Errorf("server: ITERION_SESSION_TOKEN is required in cloud mode — set the env var or override with ITERION_DISABLE_AUTH=true")
		}
		logger.Warn("server: ITERION_DISABLE_AUTH set — /api/* endpoints are unauthenticated; do not expose the server publicly")
	}
	srv := server.New(server.Config{
		Port:            serverOpts.port,
		Bind:            serverOpts.bind,
		WorkDir:         serverOpts.dir,
		Store:           st,
		LaunchPublisher: pub,
		EventSource:     eventSrc,
		Mode:            string(iterconfig.ModeCloud),
		SessionToken:    cfg.Server.SessionToken,
		// /readyz pings each dependency under a 1s deadline so kubelet
		// readiness probes flip to "not ready" the moment a backend
		// drops, instead of returning 200 against a stub.
		ReadinessChecks: map[string]server.ReadinessCheck{
			"mongo": st.Ping,
			"nats":  natsConn.Ping,
			"s3":    bc.Ping,
		},
	}, logger)

	// Prometheus metrics on a dedicated port (plan §F T-40). Bound
	// synchronously so a port-conflict surfaces at boot, not later.
	mreg := metrics.New()
	metricsAddr := fmt.Sprintf(":%d", cfg.Metrics.Port)
	metricsSrv, err := mreg.StartServer(metricsAddr, logger)
	if err != nil {
		return fmt.Errorf("server: start metrics: %w", err)
	}
	defer func() { _ = metrics.ShutdownServer(metricsSrv) }()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-rootCtx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
