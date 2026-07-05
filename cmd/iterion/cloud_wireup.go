package main

import (
	"context"
	"time"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// newCloudBlob builds the S3-backed blob client used by every
// cloud-mode subcommand (runner, server, migrate, migrate preflight).
// Callers MUST defer bc.Close() — the helper does NOT defer for them
// so close ordering at each callsite is preserved unchanged.
//
// The returned error is bare; each caller wraps it with their own
// subcommand-scoped prefix (e.g. "runner: build blob client: %w").
func newCloudBlob(ctx context.Context, cfg iterconfig.S3Config) (*blob.S3Client, error) {
	return blob.NewS3(ctx, blob.Config{
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		Bucket:          cfg.Bucket,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		UsePathStyle:    cfg.UsePathStyle,
	})
}

// newCloudMongoStore builds the Mongo-backed cloud store used by
// runner / server / migrate. The optional lockProv is non-nil only on
// the runner (server + migrate publish/read; only runner pods take
// distributed leases via NATS KV).
//
// Callers MUST defer the store's Close themselves (with their own
// timeout context) so close ordering stays under each subcommand's
// control. The returned error is bare; each caller wraps it.
func newCloudMongoStore(
	ctx context.Context,
	cfg iterconfig.MongoConfig,
	bc blob.Client,
	logger *iterlog.Logger,
	lockProv mongostore.LockProvider,
) (*mongostore.Store, error) {
	return mongostore.New(ctx, mongostore.Config{
		URI:           cfg.URI,
		Database:      cfg.DB,
		EventsTTLDays: cfg.EventsTTLDays,
		Logger:        logger,
		Blob:          bc,
		LockProvider:  lockProv,
	})
}

// closeCloudStoreWithTimeout closes a cloud Mongo store, bounding the
// close call so a slow/unreachable Mongo can't hang shutdown.
func closeCloudStoreWithTimeout(ms *mongostore.Store) {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	_ = ms.Close(closeCtx)
}
