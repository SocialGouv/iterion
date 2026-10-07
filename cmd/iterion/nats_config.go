package main

import (
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runner"
)

// natsConfig is the queue connection the server and the runner both open.
// They must pass the same values: a field only one side passes is silently
// defaulted for the other, and the two then disagree about the same broker —
// the lease TTL once did. The server's orphan sweeper reads its cutoff off
// its own connection (RedeliveryWindow), the runner spaces its retries off
// its own.
func natsConfig(cfg iterconfig.Config, logger *iterlog.Logger) natsq.Config {
	return natsq.Config{
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
		Pool:                cfg.Runner.Pool,
		LeaseUnwindCeiling:  runner.LeaseUnwindCeiling,
		Logger:              logger,
	}
}
