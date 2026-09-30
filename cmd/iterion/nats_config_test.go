package main

import (
	"testing"
	"time"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// TestNATSConfig_carriesTheLeaseUnwindCeiling: the connection the server and
// the runner both open carries the ceiling a held lease's retries are spread
// over — the server's orphan sweeper reads its cutoff off that connection —
// with the configured lease TTL.
func TestNATSConfig_carriesTheLeaseUnwindCeiling(t *testing.T) {
	var cfg iterconfig.Config
	cfg.Runner.LockTTL = 90 * time.Second
	c := natsConfig(cfg, nil)
	if c.LeaseUnwindCeiling != runtime.LeaseUnwindCeiling || c.LockTTL != 90*time.Second {
		t.Fatalf("natsConfig: lease unwind ceiling %v, lock TTL %v; want %v, 1m30s", c.LeaseUnwindCeiling, c.LockTTL, runtime.LeaseUnwindCeiling)
	}
}
