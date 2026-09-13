package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	PortCapabilityVersion   = 1
	PortCensusPrefix        = "ports-v1.census."
	PortCapabilityInterval  = 20 * time.Second
	PortCapabilityMaxAge    = time.Minute
	PortCapabilityRetention = 24 * time.Hour
	portCensusMaxEntries    = 10000
)

// PortInstanceCapability corroborates the operator's closed credential and
// immutable-build inventory. It cannot make an unknown holder compatible or
// establish credential custody by itself. No credentials belong in this data.
type PortInstanceCapability struct {
	Version          int    `json:"version"`
	Principal        string `json:"principal"`
	Instance         string `json:"instance"`
	BuildDigest      string `json:"build_digest"`
	CapabilityDigest string `json:"capability_digest"`
	StoreIdentity    string `json:"store_identity"`
	Account          string `json:"account"`
	Stream           string `json:"stream"`
	Consumer         string `json:"consumer"`
	QueueVersion     int    `json:"queue_version"`
	RunnerEpoch      uint64 `json:"runner_epoch"`
	AuthorityEpoch   uint64 `json:"authority_epoch"`
}

// PortCensusKey deliberately supports single NATS subject tokens. A configured
// principal with another spelling is unsupported, rather than being silently
// remapped to an ACL scope that an operator might interpret differently.
func PortCensusKey(principal, instance string) (string, error) {
	for _, token := range []string{principal, instance} {
		if len(token) == 0 || len(token) > 128 || strings.Trim(token, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			return "", fmt.Errorf("queue/nats: census identity must be a non-empty alphanumeric, dash or underscore token of at most 128 bytes")
		}
	}
	return PortCensusPrefix + principal + "." + instance, nil
}

func (p PortInstanceCapability) Validate() error {
	if _, err := PortCensusKey(p.Principal, p.Instance); err != nil {
		return err
	}
	digest := strings.TrimPrefix(p.BuildDigest, "sha256:")
	if p.Version != PortCapabilityVersion || !strings.HasPrefix(p.BuildDigest, "sha256:") ||
		len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" ||
		len(p.CapabilityDigest) != 64 || strings.Trim(p.CapabilityDigest, "0123456789abcdef") != "" ||
		p.StoreIdentity == "" || p.Account == "" || p.Stream == "" || p.Consumer == "" ||
		p.QueueVersion != queue.SchemaVersion || p.AuthorityEpoch == 0 {
		return fmt.Errorf("queue/nats: incomplete or unsupported instance capability")
	}
	return nil
}

type PortCapabilityObservation struct {
	Capability PortInstanceCapability `json:"capability"`
	Revision   uint64                 `json:"revision"`
	RecordedAt time.Time              `json:"recorded_at"`
}

// Fresh uses the broker-assigned timestamp, not a process-supplied clock.
func (o PortCapabilityObservation) Fresh(now time.Time) bool {
	return !o.RecordedAt.IsZero() && !now.Before(o.RecordedAt) && now.Before(o.RecordedAt.Add(PortCapabilityMaxAge))
}

func (c *Conn) portCensusKV() (jetstream.KeyValue, error) {
	if c == nil {
		return nil, fmt.Errorf("queue/nats: census connection not initialised")
	}
	kv, ok := c.rolloutKV.(jetstream.KeyValue)
	if !ok {
		return nil, fmt.Errorf("queue/nats: census rollout KV not initialised")
	}
	return kv, nil
}

func (c *Conn) PublishPortCapability(ctx context.Context, p PortInstanceCapability) (uint64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	kv, err := c.portCensusKV()
	if err != nil {
		return 0, err
	}
	if p.Stream != c.cfg.StreamName || p.Consumer != c.cfg.ConsumerName || p.RunnerEpoch != c.cfg.RunnerEpoch {
		return 0, fmt.Errorf("queue/nats: census capability does not match connected queue scope")
	}
	key, err := PortCensusKey(p.Principal, p.Instance)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	return kv.Put(ctx, key, raw)
}

// DeletePortCapability only removes the caller's last successful write. A
// stopped process cannot delete a later publication that reused its identity.
func (c *Conn) DeletePortCapability(ctx context.Context, principal, instance string, revision uint64) error {
	key, err := PortCensusKey(principal, instance)
	if err != nil {
		return err
	}
	if revision == 0 {
		return fmt.Errorf("queue/nats: census deletion requires a revision")
	}
	kv, err := c.portCensusKV()
	if err != nil {
		return err
	}
	return kv.Delete(ctx, key, jetstream.LastRevision(revision))
}

// portCensusEntries waits for the watch's explicit initial-snapshot marker.
// A closed watch/connection or timeout before that marker is not an empty or
// complete census. Retain deletion markers for bounded garbage collection.
func (c *Conn) portCensusEntries(ctx context.Context) ([]jetstream.KeyValueEntry, error) {
	kv, err := c.portCensusKV()
	if err != nil {
		return nil, err
	}
	watch, err := kv.Watch(ctx, PortCensusPrefix+">")
	if err != nil {
		return nil, err
	}
	defer func() { _ = watch.Stop() }()
	entries := make(map[string]jetstream.KeyValueEntry)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case entry, ok := <-watch.Updates():
			if !ok {
				return nil, fmt.Errorf("queue/nats: census watch ended before completing snapshot")
			}
			if entry == nil {
				result := make([]jetstream.KeyValueEntry, 0, len(entries))
				for _, e := range entries {
					result = append(result, e)
				}
				slices.SortFunc(result, func(a, b jetstream.KeyValueEntry) int { return strings.Compare(a.Key(), b.Key()) })
				return result, nil
			}
			if !strings.HasPrefix(entry.Key(), PortCensusPrefix) {
				return nil, fmt.Errorf("queue/nats: census watch returned a key outside its scope")
			}
			if len(entry.Value()) > 16*1024 {
				return nil, fmt.Errorf("queue/nats: oversized census entry")
			}
			entries[entry.Key()] = entry
			if len(entries) > portCensusMaxEntries {
				return nil, fmt.Errorf("queue/nats: census exceeds supported inventory size")
			}
		}
	}
}

func (c *Conn) PortCapabilities(ctx context.Context) ([]PortCapabilityObservation, error) {
	entries, err := c.portCensusEntries(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]PortCapabilityObservation, 0, len(entries))
	for _, entry := range entries {
		if entry.Operation() != jetstream.KeyValuePut {
			continue
		}
		if len(entry.Value()) > 16*1024 {
			return nil, fmt.Errorf("queue/nats: oversized census entry")
		}
		var p PortInstanceCapability
		decoder := json.NewDecoder(bytes.NewReader(entry.Value()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&p); err != nil {
			// Parser errors may quote an untrusted payload. Keep diagnostics
			// structural so unexpected credentials cannot escape into logs.
			return nil, fmt.Errorf("queue/nats: malformed census entry")
		}
		if decoder.Decode(new(any)) != io.EOF || p.Validate() != nil {
			return nil, fmt.Errorf("queue/nats: invalid census capability")
		}
		key, err := PortCensusKey(p.Principal, p.Instance)
		if err != nil || key != entry.Key() {
			return nil, fmt.Errorf("queue/nats: census key disagrees with declared identity")
		}
		result = append(result, PortCapabilityObservation{Capability: p, Revision: entry.Revision(), RecordedAt: entry.Created()})
	}
	return result, nil
}

// PrunePortCapabilities removes only old census subjects, including graceful
// shutdown tombstones. A subject-and-sequence bounded stream purge removes no
// concurrently refreshed value and never touches monotonic rollout keys. This
// method belongs to the elected authority, not ordinary runner credentials.
func (c *Conn) PrunePortCapabilities(ctx context.Context, now time.Time) error {
	entries, err := c.portCensusEntries(ctx)
	if err != nil {
		return err
	}
	return c.prunePortCensusEntries(ctx, entries, now)
}

func (c *Conn) prunePortCensusEntries(ctx context.Context, entries []jetstream.KeyValueEntry, now time.Time) error {
	stream, err := c.js.Stream(ctx, "KV_"+c.cfg.RolloutKVBucket)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Created().IsZero() || now.Before(entry.Created().Add(PortCapabilityRetention)) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(entry.Key(), PortCensusPrefix), ".")
		if len(parts) != 2 {
			return fmt.Errorf("queue/nats: invalid census key cannot be pruned")
		}
		if _, err := PortCensusKey(parts[0], parts[1]); err != nil {
			return err
		}
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject("$KV."+c.cfg.RolloutKVBucket+"."+entry.Key()),
			jetstream.WithPurgeSequence(entry.Revision()+1)); err != nil {
			return err
		}
	}
	return nil
}

// RunPortCapabilityHeartbeat owns one process-instance key until shutdown.
// It is opt-in at application construction; ordinary filesystem use has no
// background connection or census. Temporary failures cannot extend freshness.
func (c *Conn) RunPortCapabilityHeartbeat(ctx context.Context, p PortInstanceCapability, onError func(error)) error {
	if err := p.Validate(); err != nil {
		return err
	}
	var revision uint64
	report := func(err error) {
		if err != nil && onError != nil {
			onError(err)
		}
	}
	defer func() {
		if revision == 0 {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := c.DeletePortCapability(cleanup, p.Principal, p.Instance, revision)
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			report(err)
		}
	}()
	ticker := time.NewTicker(PortCapabilityInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		written, err := c.PublishPortCapability(opCtx, p)
		cancel()
		if err == nil {
			revision = written
		} else {
			report(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
