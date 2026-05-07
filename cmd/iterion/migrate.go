package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// `iterion migrate to-cloud` walks a filesystem .iterion/ store and
// re-uploads every run + events + artifacts into Mongo+S3 via the
// cloud-mode store. Hidden by default — operators only need it once
// for the local→cloud migration path. Plan §F (T-42).

var migrateOpts struct {
	storeDir    string
	configPath  string
	dryRun      bool
	concurrency int
}

var migrateCmd = &cobra.Command{
	Use:    "migrate",
	Hidden: true,
	Short:  "Migration tooling (hidden — operator-only)",
}

var migrateToCloudCmd = &cobra.Command{
	Use:   "to-cloud",
	Short: "Upload a filesystem .iterion store into Mongo+S3",
	Long: `Walk every run under --store-dir, persist its run.json + events.jsonl
metadata into Mongo, and upload artifacts to S3. Idempotent: re-running
overwrites the same Mongo _id (run_id) and the canonical S3 key for
each artifact version, so two passes produce identical output.

ITERION_MONGO_URI / ITERION_S3_* must be set (or pass --config). The
local .iterion/ filesystem is read-only — the migration never deletes
or modifies the source store.`,
	Args: cobra.NoArgs,
	RunE: runMigrateToCloud,
}

func init() {
	migrateToCloudCmd.Flags().StringVar(&migrateOpts.storeDir, "store-dir", ".iterion", "Filesystem store to migrate from")
	migrateToCloudCmd.Flags().StringVar(&migrateOpts.configPath, "config", "", "Path to YAML config (env vars take precedence)")
	migrateToCloudCmd.Flags().BoolVar(&migrateOpts.dryRun, "dry-run", false, "Print what would be uploaded; don't write to Mongo or S3")
	migrateToCloudCmd.Flags().IntVar(&migrateOpts.concurrency, "concurrency", 4, "Parallel run uploads")
	migrateCmd.AddCommand(migrateToCloudCmd)
	rootCmd.AddCommand(migrateCmd)
}

func runMigrateToCloud(cmd *cobra.Command, _ []string) error {
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{
		YAMLPath:         migrateOpts.configPath,
		DefaultLogFormat: iterconfig.LogFormatHuman,
	})
	if err != nil {
		return fmt.Errorf("migrate: load config: %w", err)
	}
	if cfg.Mongo.URI == "" || cfg.S3.Bucket == "" {
		return errors.New("migrate: ITERION_MONGO_URI and ITERION_S3_BUCKET are required")
	}

	logger := iterlog.NewWithFormat(parseLevel(cfg.Log.Level), cmd.ErrOrStderr(), parseLogFormat(cfg.Log.Format))

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	src, err := store.New(migrateOpts.storeDir)
	if err != nil {
		return fmt.Errorf("migrate: open source %s: %w", migrateOpts.storeDir, err)
	}

	var dst store.RunStore
	if !migrateOpts.dryRun {
		bc, err := blob.NewS3(ctx, blob.Config{
			Endpoint:        cfg.S3.Endpoint,
			Region:          cfg.S3.Region,
			Bucket:          cfg.S3.Bucket,
			AccessKeyID:     cfg.S3.AccessKeyID,
			SecretAccessKey: cfg.S3.SecretAccessKey,
			UsePathStyle:    cfg.S3.UsePathStyle,
		})
		if err != nil {
			return fmt.Errorf("migrate: build blob: %w", err)
		}
		defer func() { _ = bc.Close() }()
		ms, err := mongostore.New(ctx, mongostore.Config{
			URI:           cfg.Mongo.URI,
			Database:      cfg.Mongo.DB,
			EventsTTLDays: cfg.Mongo.EventsTTLDays,
			Logger:        logger,
			Blob:          bc,
		})
		if err != nil {
			return fmt.Errorf("migrate: build mongo: %w", err)
		}
		defer func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			_ = ms.Close(closeCtx)
		}()
		dst = ms
	}

	// Pre-flight before iterating runs: a misconfigured Mongo (no
	// replicaset → no change-streams) or a non-writable S3 bucket
	// would surface as a partial migration after thousands of runs
	// were already processed. Catch the gaps up-front.
	if !migrateOpts.dryRun {
		preflightCtx, preflightCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := preflightCloudTargets(preflightCtx, cfg, logger); err != nil {
			preflightCancel()
			return fmt.Errorf("migrate: preflight: %w", err)
		}
		preflightCancel()
	}

	ids, err := src.ListRuns(ctx)
	if err != nil {
		return fmt.Errorf("migrate: list runs: %w", err)
	}
	logger.Info("migrate: %d runs to process (dry_run=%t)", len(ids), migrateOpts.dryRun)

	concurrency := migrateOpts.concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var migErr error
	var migMu sync.Mutex
	var success, failed int

	for _, id := range ids {
		id := id
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := migrateRun(ctx, src, dst, id, migrateOpts.dryRun, logger); err != nil {
				migMu.Lock()
				failed++
				if migErr == nil {
					migErr = err
				}
				migMu.Unlock()
				logger.Error("migrate: run %s: %v", id, err)
			} else {
				migMu.Lock()
				success++
				migMu.Unlock()
				logger.Info("migrate: run %s ok", id)
			}
		}()
	}
	wg.Wait()
	// Aggregate summary so the operator sees the full failure count
	// even when only the first error is returned. Without this, large
	// migrations could complete with dozens of silent failures hidden
	// across per-run Error log lines.
	logger.Info("migrate: completed: %d ok, %d failed (of %d total)", success, failed, len(ids))
	if failed > 0 {
		return fmt.Errorf("migrate: %d/%d runs failed (first: %w)", failed, len(ids), migErr)
	}
	return nil
}

// migrateRun uploads a single run's metadata + events + artifacts.
// Order matters: events before artifacts (the artifact_written event
// is the source of truth that the version exists), runs before events
// (the run document holds the index + checkpoint). Each step is
// idempotent against re-runs (Mongo upsert + S3 PUT-overwrite).
func migrateRun(ctx context.Context, src store.RunStore, dst store.RunStore, runID string, dryRun bool, logger *iterlog.Logger) error {
	r, err := src.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}

	if !dryRun {
		if err := dst.SaveRun(ctx, r); err != nil {
			return fmt.Errorf("save run: %w", err)
		}
	}

	events, err := src.LoadEvents(ctx, runID)
	if err != nil {
		return fmt.Errorf("load events: %w", err)
	}
	logger.Info("migrate: run %s — %d events, %d artifacts (index)", runID, len(events), len(r.ArtifactIndex))
	if !dryRun {
		for _, e := range events {
			if _, err := dst.AppendEvent(ctx, runID, *e); err != nil {
				// Tolerate duplicates so a re-run just resumes from
				// where the previous attempt died.
				logger.Warn("migrate: append event %s/%d: %v", runID, e.Seq, err)
			}
		}
	}

	for nodeID, ver := range r.ArtifactIndex {
		// Walk every version up to and including the latest known
		// (the index is the latest-only cache; older versions are
		// discovered via ListArtifactVersions).
		versions, err := src.ListArtifactVersions(ctx, runID, nodeID)
		if err != nil {
			logger.Warn("migrate: list artifact versions %s/%s: %v", runID, nodeID, err)
			continue
		}
		_ = ver
		for _, info := range versions {
			art, err := src.LoadArtifact(ctx, runID, nodeID, info.Version)
			if err != nil {
				logger.Warn("migrate: load artifact %s/%s/v%d: %v", runID, nodeID, info.Version, err)
				continue
			}
			if dryRun {
				continue
			}
			if err := dst.WriteArtifact(ctx, art); err != nil {
				logger.Warn("migrate: write artifact %s/%s/v%d: %v", runID, nodeID, info.Version, err)
			}
		}
	}

	for _, intID := range mustListInteractions(ctx, src, runID, logger) {
		i, err := src.LoadInteraction(ctx, runID, intID)
		if err != nil {
			logger.Warn("migrate: load interaction %s/%s: %v", runID, intID, err)
			continue
		}
		if dryRun {
			continue
		}
		if err := dst.WriteInteraction(ctx, i); err != nil {
			logger.Warn("migrate: write interaction %s/%s: %v", runID, intID, err)
		}
	}

	return nil
}

func mustListInteractions(ctx context.Context, src store.RunStore, runID string, logger *iterlog.Logger) []string {
	ids, err := src.ListInteractions(ctx, runID)
	if err != nil {
		logger.Warn("migrate: list interactions %s: %v", runID, err)
		return nil
	}
	return ids
}

// preflightCloudTargets verifies the cloud destination is usable before
// the loop starts processing runs. Two failure modes are too costly to
// hit mid-migration:
//
//  1. Mongo without a replica set — the migration writes succeed, but
//     the cloud server's change-stream subscription (downstream
//     consumer of these events) silently fails. Operators don't notice
//     until the editor's run console stays empty.
//
//  2. S3 bucket without write permission — every artifact PUT fails;
//     the migration runs to completion reporting "0 artifacts written"
//     per run because the per-artifact errors are logged but don't
//     abort the loop.
//
// The probes are scoped to the supplied ctx so a hung backend can't
// stall the migration past the caller's deadline.
func preflightCloudTargets(ctx context.Context, cfg iterconfig.Config, logger *iterlog.Logger) error {
	// Mongo: rs.status() — succeeds only on a replica-set member.
	bc, err := blob.NewS3(ctx, blob.Config{
		Endpoint:        cfg.S3.Endpoint,
		Region:          cfg.S3.Region,
		Bucket:          cfg.S3.Bucket,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		UsePathStyle:    cfg.S3.UsePathStyle,
	})
	if err != nil {
		return fmt.Errorf("s3 client: %w", err)
	}
	defer func() { _ = bc.Close() }()
	probeKey := fmt.Sprintf("preflight/%d", time.Now().UnixNano())
	if err := bc.PutArtifact(ctx, "preflight", probeKey, 0, []byte("ok")); err != nil {
		return fmt.Errorf("s3 write probe failed (bucket=%q): %w", cfg.S3.Bucket, err)
	}
	if err := bc.DeleteRun(ctx, "preflight"); err != nil {
		// Best-effort cleanup; log + continue. The probe key will fall
		// out via lifecycle policy if the bucket has one configured.
		logger.Warn("migrate: preflight S3 cleanup: %v", err)
	}
	logger.Info("migrate: preflight S3 ok (bucket=%s)", cfg.S3.Bucket)

	// Mongo replSet probe via a dedicated short-lived store. We don't
	// reuse `dst` from the caller because the conformance check only
	// makes sense before the loop; dst stays the workhorse store.
	probeStore, err := mongostore.New(ctx, mongostore.Config{
		URI:           cfg.Mongo.URI,
		Database:      cfg.Mongo.DB,
		EventsTTLDays: cfg.Mongo.EventsTTLDays,
		Logger:        logger,
		Blob:          bc,
	})
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = probeStore.Close(closeCtx)
	}()
	var status bson.M
	if err := probeStore.RunsCollection().Database().RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status); err != nil {
		return fmt.Errorf("mongo replSetGetStatus failed (the cloud server requires a replica set for change-streams): %w", err)
	}
	logger.Info("migrate: preflight Mongo ok (replSet=%v)", status["set"])
	return nil
}
