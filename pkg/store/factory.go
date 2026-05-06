package store

import (
	"context"
	"fmt"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// CloudOpener is the optional hook that wires the cloud backend
// (Mongo + S3) without forcing pkg/store to import them directly. The
// cloud build registers an opener via init() in pkg/store/cloud
// (or, in production, pkg/cli's bootstrap path); leaving the opener
// nil keeps pkg/store dependency-free for CLI builds that never touch
// the cloud backend.
//
// See cloud-ready plan §F (T-19).
type CloudOpener func(ctx context.Context, cfg OpenConfig) (RunStore, error)

var cloudOpener CloudOpener

// RegisterCloudOpener installs the cloud-mode store factory. Idempotent:
// the last call wins. Designed to be invoked from a single bootstrap
// site (cmd/iterion or a pkg/store/cloud init) so a binary that omits
// the cloud build still compiles and runs in local mode.
func RegisterCloudOpener(opener CloudOpener) {
	cloudOpener = opener
}

// OpenConfig is the dispatch input shared between local and cloud
// modes. The same struct is used regardless of mode; cloud-only
// fields are simply ignored when Mode == "local".
//
// See cloud-ready plan §C.1.
type OpenConfig struct {
	// Mode selects between filesystem (default) and Mongo+S3 backends.
	// Empty string is treated as "local" so existing CLI paths that
	// never set this field keep working.
	Mode string

	// StoreDir is required when Mode == "local"; ignored otherwise.
	StoreDir string

	// Cloud-only fields. Reserved for the Mongo+S3 implementation
	// that lands in plan §F T-17/T-19. Setting them while Mode ==
	// "local" is a no-op (and a forward-compatible warning point).
	MongoURI      string
	MongoDB       string
	NATSURL       string
	NATSKVBucket  string
	S3            FactoryS3Config
	EventsTTLDays int

	Logger *iterlog.Logger
}

// FactoryS3Config holds the S3/MinIO connection settings carried by
// OpenConfig. Distinct from blob.S3Config so the store package does
// not depend on the (yet-to-be-added) blob package — both will
// eventually share a single config object via plan §F T-16/T-19.
type FactoryS3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool
}

// Open dispatches on cfg.Mode and returns a backend-appropriate
// RunStore. Today only the local branch is wired; the cloud branch
// returns an explicit "not implemented" error so callers can surface
// the missing build to operators rather than silently falling back.
//
// See cloud-ready plan §C.1, §F T-08, T-19.
func Open(ctx context.Context, cfg OpenConfig) (RunStore, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = "local"
	}
	switch mode {
	case "local":
		return openLocal(cfg)
	case "cloud":
		return openCloud(ctx, cfg)
	default:
		return nil, fmt.Errorf("store: unknown mode %q (want local|cloud)", cfg.Mode)
	}
}

func openLocal(cfg OpenConfig) (RunStore, error) {
	if cfg.StoreDir == "" {
		return nil, fmt.Errorf("store: local mode requires StoreDir")
	}
	opts := []StoreOption{}
	if cfg.Logger != nil {
		opts = append(opts, WithLogger(cfg.Logger))
	}
	return New(cfg.StoreDir, opts...)
}

// openCloud dispatches to the registered cloud opener (typically the
// Mongo+S3 backend in pkg/store/cloud). When no opener is registered
// the call fails fast so an operator who configured ITERION_MODE=cloud
// against a binary built without the cloud package observes the gap
// at boot, not after a few runs have written to /tmp.
func openCloud(ctx context.Context, cfg OpenConfig) (RunStore, error) {
	if cloudOpener == nil {
		return nil, fmt.Errorf("store: cloud opener not registered (this binary was built without the cloud backend)")
	}
	return cloudOpener(ctx, cfg)
}
