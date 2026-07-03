package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Quota accounting for the FS adapter.
//
// All memory writes route through a single coarse, cross-process
// advisory lock at <base>/.quota/.lock. Under it we read the per-space
// and global-aggregate usage sidecars, check both ceilings, write the
// document, then update both counters — so a write never lands when it
// would exceed a cap, and concurrent writers (across goroutines or
// processes sharing the data dir) serialise correctly. Memory writes
// are low-frequency checkpoints, so the coarse lock is cheap.
//
// Sidecars live under <base>/.quota/ — a sibling of projects/, never
// inside a space dir — so they never appear in memory_list / the
// auto-index. The aggregate ("org total" in single-tenant local mode)
// spans the whole memory tree under <base>.

const quotaSidecarVersion = 1

// usageSidecar is the on-disk usage record. QuotaBytes == 0 means
// "use the env/default ceiling", so an env change takes effect without
// rewriting every sidecar; an explicit per-space override (admin /
// SetQuota) persists a non-zero value.
type usageSidecar struct {
	Version    int    `json:"version"`
	RefID      string `json:"ref_id,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	Name       string `json:"name,omitempty"`
	UsedBytes  int64  `json:"used_bytes"`
	QuotaBytes int64  `json:"quota_bytes,omitempty"`
}

func quotaDir(base string) string      { return filepath.Join(base, ".quota") }
func quotaLockPath(base string) string { return filepath.Join(quotaDir(base), ".lock") }
func aggregatePath(base string) string { return filepath.Join(quotaDir(base), "aggregate.json") }

func spaceSidecarPath(base string, ref knowledge.SpaceRef) string {
	return filepath.Join(quotaDir(base), "spaces", knowledge.ChecksumHex([]byte(ref.ID()))+".json")
}

// envQuota reads a non-negative int64 byte ceiling from an env var,
// falling back to def when unset or unparseable.
func envQuota(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func aggregateQuota() int64 {
	return envQuota("ITERION_MEMORY_QUOTA_ORG_TOTAL", knowledge.DefaultOrgAggregateQuota)
}

func spaceQuotaFor(v knowledge.Visibility) int64 {
	return envQuota("ITERION_MEMORY_QUOTA_"+strings.ToUpper(string(v)), knowledge.DefaultQuotaFor(v))
}

func maxDocumentSize() int64 {
	return envQuota("ITERION_MEMORY_MAX_DOC", knowledge.DefaultMaxDocumentSize)
}

// effectiveQuota prefers an explicit per-record override (>0), else the
// env/default ceiling.
func effectiveQuota(override, def int64) int64 {
	if override > 0 {
		return override
	}
	return def
}

func readSidecar(path string) (usageSidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return usageSidecar{Version: quotaSidecarVersion}, nil
		}
		return usageSidecar{}, err
	}
	var s usageSidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return usageSidecar{}, fmt.Errorf("memory: corrupt usage sidecar %s: %w", path, err)
	}
	return s, nil
}

func writeSidecar(path string, s usageSidecar) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	s.Version = quotaSidecarVersion
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(path, data, 0o644)
}

// acquireQuotaLock takes the global quota lock, blocking (with a short
// retry) up to a timeout. store.AcquireFileLock is non-blocking, so we
// spin: under the rare contention of two concurrent memory writes this
// just waits its turn instead of failing.
func acquireQuotaLock(base string) (store.RunLock, error) {
	lp := quotaLockPath(base)
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := store.AcquireFileLock(lp, "memory quota")
		if err == nil {
			return lock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("memory: quota lock busy: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkCeilings returns a *knowledge.QuotaError when adding delta bytes
// would breach the per-space sub-cap or the org aggregate ceiling. A
// non-positive delta (an overwrite that shrinks, or a delete) never
// breaches and returns nil. Must be called while holding the quota lock.
func checkCeilings(ref knowledge.SpaceRef, space, agg usageSidecar, delta int64) error {
	if delta <= 0 {
		return nil
	}
	// A per-space quota of 0 means "no sub-cap" (only the org aggregate
	// applies) — e.g. the global space. It never means "deny".
	if spaceQ := effectiveQuota(space.QuotaBytes, spaceQuotaFor(ref.Visibility)); spaceQ > 0 && space.UsedBytes+delta > spaceQ {
		return &knowledge.QuotaError{Aggregate: false, Used: space.UsedBytes, Delta: delta, Quota: spaceQ}
	}
	aggQ := effectiveQuota(agg.QuotaBytes, aggregateQuota())
	if agg.UsedBytes+delta > aggQ {
		return &knowledge.QuotaError{Aggregate: true, Used: agg.UsedBytes, Delta: delta, Quota: aggQ}
	}
	return nil
}

// commitDelta applies a byte delta to both counters and persists them.
// Must be called while holding the quota lock, AFTER the document write
// succeeded.
func commitDelta(base string, ref knowledge.SpaceRef, space, agg usageSidecar, delta int64) error {
	space.UsedBytes = clampNonNeg(space.UsedBytes + delta)
	space.RefID, space.Visibility, space.Name = ref.ID(), string(ref.Visibility), ref.Name
	agg.UsedBytes = clampNonNeg(agg.UsedBytes + delta)
	if err := writeSidecar(spaceSidecarPath(base, ref), space); err != nil {
		return err
	}
	return writeSidecar(aggregatePath(base), agg)
}

func clampNonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
