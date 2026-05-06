// Package cloud wires the Mongo + S3 cloud backend into the store
// factory. Importing this package (typically from the cmd/iterion or
// pkg/cli bootstrap) registers the cloud opener via init(), so callers
// only need a blank import to enable ITERION_MODE=cloud.
//
// Splitting the wiring into its own package keeps pkg/store free of
// AWS / Mongo dependencies for CLI builds that never need cloud mode.
//
// Cloud-ready plan §F T-19.
package cloud

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

func init() {
	store.RegisterCloudOpener(open)
}

// open is the cloud-mode RunStore factory. It wires the S3 blob
// client, the Mongo run/event/interaction store, and joins them
// together so WriteArtifact can PUT through the blob and then insert
// the event in Mongo (plan §D.6).
func open(ctx context.Context, cfg store.OpenConfig) (store.RunStore, error) {
	if cfg.MongoURI == "" {
		return nil, fmt.Errorf("store/cloud: ITERION_MONGO_URI is required")
	}
	if cfg.S3.Bucket == "" {
		return nil, fmt.Errorf("store/cloud: ITERION_S3_BUCKET is required")
	}

	bc, err := blob.NewS3(ctx, blob.Config{
		Endpoint:        cfg.S3.Endpoint,
		Region:          cfg.S3.Region,
		Bucket:          cfg.S3.Bucket,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		UsePathStyle:    cfg.S3.UsePathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("store/cloud: build blob client: %w", err)
	}

	ms, err := mongostore.New(ctx, mongostore.Config{
		URI:           cfg.MongoURI,
		Database:      cfg.MongoDB,
		EventsTTLDays: cfg.EventsTTLDays,
		Logger:        cfg.Logger,
		Blob:          bc,
	})
	if err != nil {
		return nil, err
	}
	return ms, nil
}
