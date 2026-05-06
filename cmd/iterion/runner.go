package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runner"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/spf13/cobra"
)

func parseLevel(s string) iterlog.Level {
	if l, err := iterlog.ParseLevel(s); err == nil {
		return l
	}
	return iterlog.LevelInfo
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

	logger := iterlog.New(parseLevel(cfg.Log.Level), cmd.ErrOrStderr())
	logger.Info("runner: starting")

	rootCtx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 1. NATS layer — provides the queue + KV lock bucket.
	natsConn, err := natsq.Connect(rootCtx, natsq.Config{
		URL:        cfg.NATS.URL,
		StreamName: cfg.NATS.Stream,
		DLQStream:  cfg.NATS.DLQStream,
		KVBucket:   cfg.NATS.KVBucket,
		LockTTL:    cfg.Runner.LockTTL,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("runner: connect NATS: %w", err)
	}
	defer natsConn.Close()

	// 2. Blob (S3 / MinIO) — backs WriteArtifact/LoadArtifact.
	bc, err := blob.NewS3(rootCtx, blob.Config{
		Endpoint:        cfg.S3.Endpoint,
		Region:          cfg.S3.Region,
		Bucket:          cfg.S3.Bucket,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		UsePathStyle:    cfg.S3.UsePathStyle,
	})
	if err != nil {
		return fmt.Errorf("runner: build blob client: %w", err)
	}

	// 3. Mongo store with NATS-KV-backed lock provider so LockRun
	//    returns a real distributed lease (vs the no-op in
	//    server-side cloud store usage).
	runnerID, _ := os.Hostname()
	lockProv := natsq.NewLockProvider(natsConn, runnerID)
	st, err := mongostore.New(rootCtx, mongostore.Config{
		URI:           cfg.Mongo.URI,
		Database:      cfg.Mongo.DB,
		EventsTTLDays: cfg.Mongo.EventsTTLDays,
		Logger:        logger,
		Blob:          bc,
		LockProvider:  lockProv,
	})
	if err != nil {
		return fmt.Errorf("runner: build mongo store: %w", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = st.Close(closeCtx)
	}()

	// 4. Runner loop.
	r, err := runner.New(rootCtx, runner.Config{
		NATS:              natsConn,
		Store:             st,
		RunnerID:          runnerID,
		WorkDir:           cfg.Runner.WorkDir,
		HeartbeatInterval: cfg.Runner.Heartbeat,
		Logger:            logger,
	})
	if err != nil {
		return fmt.Errorf("runner: build: %w", err)
	}

	// SIGTERM handling: cancel the loop ctx, then wait up to grace
	// for the in-flight run to checkpoint + nak.
	go func() {
		<-rootCtx.Done()
		logger.Info("runner: shutdown signal received")
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer drainCancel()
		_ = r.Shutdown(drainCtx)
	}()

	if err := r.Run(rootCtx); err != nil && err != context.Canceled {
		return fmt.Errorf("runner: loop: %w", err)
	}
	logger.Info("runner: exited cleanly")
	return nil
}
