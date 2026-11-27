package nats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// The fleet registry: one KV record per live server and runner process,
// kept fresh by a heartbeat and expired by the bucket TTL when a process dies
// without deleting it. It answers "which builds can execute a run launched
// NOW" — the population the engine floor holds a bot push against — instead
// of inferring it from the runs of the past week, where a pod that drains, or
// left days ago, keeps refusing pushes its successors can run.

// DefaultPresenceTTL is how long a record outlives its last write. The
// heartbeat writes every quarter of it (presenceInterval), so a live process
// misses three beats before its record expires.
const DefaultPresenceTTL = 60 * time.Second

// presenceKV is the KV surface the registry writes and reads values
// through. Narrow so the heartbeat, freshness and reading rules are testable
// without a broker.
type presenceKV interface {
	Put(ctx context.Context, key string, value []byte) (uint64, error)
	Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
}

// presenceKeyLister names every key that holds a message in the registry.
//
// Not the KV's ListKeys: that is a watch, and the client ends a watch whose
// subscription closes — a dropped connection — exactly as it ends a complete
// listing (it closes the updates channel, which the lister reads as the end
// marker), and it sends the end marker after the FIRST key when it could not
// read the consumer's initial pending count. Either way a partial fleet would
// read as the whole fleet, and the floor would skip a runner that can take a
// launch. A stream-info request answers the whole subject set or an error.
type presenceKeyLister func(ctx context.Context) ([]string, error)

// streamSubjects answers a stream's per-subject message counts under a
// subject filter (jsStreamSubjects in production).
type streamSubjects func(ctx context.Context, stream, filter string) (map[string]uint64, error)

// jsStreamSubjects reads a stream's per-subject state through a filtered
// stream-info request, paged by the client: one request/response per page,
// the whole subject set or an error.
func jsStreamSubjects(js jetstream.JetStream) streamSubjects {
	return func(ctx context.Context, name, filter string) (map[string]uint64, error) {
		stream, err := js.Stream(ctx, name)
		if err != nil {
			return nil, err
		}
		info, err := stream.Info(ctx, jetstream.WithSubjectFilter(filter))
		if err != nil {
			return nil, err
		}
		return info.State.Subjects, nil
	}
}

// streamKeyLister lists the registry's keys from the bucket's backing stream
// (`KV_<bucket>`), every subject under `$KV.<bucket>.`.
func streamKeyLister(subjects streamSubjects, bucket string) presenceKeyLister {
	return func(ctx context.Context) ([]string, error) {
		state, err := subjects(ctx, kvStreamName(bucket), kvSubjectPrefix(bucket)+">")
		if err != nil {
			return nil, err
		}
		return keysOfSubjects(bucket, state), nil
	}
}

func kvStreamName(bucket string) string    { return "KV_" + bucket }
func kvSubjectPrefix(bucket string) string { return "$KV." + bucket + "." }

// keysOfSubjects turns a KV stream's per-subject state into the bucket's
// keys; a subject outside the bucket's prefix is not one of its keys.
func keysOfSubjects(bucket string, subjects map[string]uint64) []string {
	prefix := kvSubjectPrefix(bucket)
	keys := make([]string, 0, len(subjects))
	for subject := range subjects {
		if key, ok := strings.CutPrefix(subject, prefix); ok && key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// presenceInterval is the heartbeat period for a bucket TTL.
func presenceInterval(ttl time.Duration) time.Duration { return ttl / 4 }

// Presence is one process's record in the fleet registry.
//
// The contract a runner relies on: Register before the consumer starts (a
// runner the registry cannot see must not take work), Fresh before each fetch
// (a record that may already have expired pauses fetching until a write lands
// again), MarkDraining once fetching has stopped, Deregister on a clean exit.
type Presence struct {
	kv       presenceKV
	key      string
	ttl      time.Duration
	interval time.Duration
	logger   *iterlog.Logger
	now      func() time.Time

	// writeGate serialises writes, so the record that lands last carries
	// the newest state: a heartbeat that snapshotted the record before
	// MarkDraining cannot overwrite the draining flag after it, and no
	// write lands after Deregister. A capacity-1 channel rather than a
	// mutex, so a write waiting behind a slow one honours its own deadline.
	writeGate chan struct{}
	closed    atomic.Bool

	mu     sync.Mutex
	member queue.FleetMember
	// lastSent is when the last SUCCESSFUL write was sent. The broker
	// stamps the record no earlier than that, so the record is known to
	// exist until lastSent + ttl.
	lastSent time.Time
	// lapsed remembers that a write failed, so the next success logs the
	// recovery once instead of every beat logging.
	lapsed bool
}

// NewPresence builds this process's registry record. instance names the
// process for an operator (the pod's hostname); a random suffix keeps two
// processes on one host — or a container restarted in the same pod before
// its predecessor's record expired — from overwriting each other. The build
// is this binary's own (appinfo.FullVersion, the string a runner stamps on
// the runs it takes), never a caller's claim about it.
func (c *Conn) NewPresence(role, instance, pool string) (*Presence, error) {
	if c == nil || c.presenceKV == nil {
		return nil, fmt.Errorf("queue/nats: fleet registry is not initialised")
	}
	if role != queue.FleetRoleServer && role != queue.FleetRoleRunner {
		return nil, fmt.Errorf("queue/nats: fleet role %q (want %q or %q)", role, queue.FleetRoleServer, queue.FleetRoleRunner)
	}
	suffix, err := randomSuffix()
	if err != nil {
		return nil, fmt.Errorf("queue/nats: fleet instance suffix: %w", err)
	}
	instance = strings.TrimSpace(instance)
	if instance == "" {
		instance = "unnamed"
	}
	instance += "-" + suffix
	member := queue.FleetMember{
		Role:      role,
		Instance:  instance,
		Build:     appinfo.FullVersion(),
		Epoch:     c.cfg.RunnerEpoch,
		Pool:      pool,
		StartedAt: time.Now().UTC(),
	}
	return newPresence(c.presenceKV, member, c.cfg.PresenceTTL, c.logger), nil
}

func newPresence(kv presenceKV, member queue.FleetMember, ttl time.Duration, logger *iterlog.Logger) *Presence {
	if ttl <= 0 {
		ttl = DefaultPresenceTTL
	}
	return &Presence{
		kv:        kv,
		key:       presenceKey(member.Role, member.Instance),
		ttl:       ttl,
		interval:  presenceInterval(ttl),
		logger:    logger,
		now:       time.Now,
		writeGate: make(chan struct{}, 1),
		member:    member,
	}
}

// acquireWrite takes the write gate, or gives up when ctx ends first.
func (p *Presence) acquireWrite(ctx context.Context) error {
	select {
	case p.writeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Presence) releaseWrite() { <-p.writeGate }

// presenceKey is `<role>.<instance>` with every character a KV key does not
// accept — and the dot, so the key keeps exactly two tokens — replaced.
func presenceKey(role, instance string) string {
	var b strings.Builder
	for _, r := range instance {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return role + "." + b.String()
}

func randomSuffix() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// Member returns the record as this process last wrote it.
func (p *Presence) Member() queue.FleetMember {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.member
}

// Register writes the record once. Its error is the caller's to act on: a
// runner that cannot be seen must not start consuming.
func (p *Presence) Register(ctx context.Context) error {
	if err := p.write(ctx); err != nil {
		return fmt.Errorf("queue/nats: register %s in the fleet registry: %w", p.key, err)
	}
	return nil
}

// Run refreshes the record every interval until ctx ends. A failed write is
// logged once per lapse; Fresh turns false when no write has landed for
// long enough that the record may be gone.
func (p *Presence) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := p.write(ctx)
		p.mu.Lock()
		wasLapsed := p.lapsed
		p.lapsed = err != nil
		p.mu.Unlock()
		switch {
		case errors.Is(err, errPresenceClosed):
			return
		case err != nil && !wasLapsed && ctx.Err() == nil:
			p.logWarn("fleet registry: heartbeat for %s failed: %v — the record expires %s after the last write; a runner stops fetching before that", p.key, err, p.ttl)
		case err == nil && wasLapsed:
			p.logInfo("fleet registry: heartbeat for %s recovered", p.key)
		}
	}
}

// MarkDraining republishes the record with Draining set. A runner calls it
// once it has stopped fetching, never before: the floor stops counting a
// draining runner, which is only true of one that takes no new run.
func (p *Presence) MarkDraining(ctx context.Context) error {
	p.mu.Lock()
	p.member.Draining = true
	p.mu.Unlock()
	err := p.write(ctx)
	if errors.Is(err, errPresenceClosed) {
		// Already deregistered — an idle runner's exit can delete the
		// record before Shutdown marks it: nothing left to mark.
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue/nats: mark %s draining: %w", p.key, err)
	}
	return nil
}

// Fresh reports whether the record is known to be in the registry with a
// margin of one heartbeat: the last successful write was sent less than
// ttl - interval ago. The margin outlasts a fetch that starts while fresh.
func (p *Presence) Fresh() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastSent.IsZero() {
		return false
	}
	return p.now().Sub(p.lastSent) < p.ttl-p.interval
}

// Deregister deletes the record on a clean exit; no write lands after it. A
// process that dies first leaves its record to the bucket TTL.
func (p *Presence) Deregister(ctx context.Context) error {
	// Closed first: whatever happens to the delete, no later write lands.
	p.closed.Store(true)
	if err := p.acquireWrite(ctx); err != nil {
		return fmt.Errorf("queue/nats: deregister %s from the fleet registry: %w", p.key, err)
	}
	defer p.releaseWrite()
	if err := p.kv.Delete(ctx, p.key); err != nil {
		return fmt.Errorf("queue/nats: deregister %s from the fleet registry: %w", p.key, err)
	}
	return nil
}

// errPresenceClosed refuses a write after Deregister.
var errPresenceClosed = errors.New("queue/nats: fleet record deregistered")

func (p *Presence) write(ctx context.Context) error {
	if err := p.acquireWrite(ctx); err != nil {
		return err
	}
	defer p.releaseWrite()
	if p.closed.Load() {
		return errPresenceClosed
	}
	p.mu.Lock()
	member := p.member
	p.mu.Unlock()
	value, err := json.Marshal(member)
	if err != nil {
		return err
	}
	sent := p.now()
	if _, err := p.kv.Put(ctx, p.key, value); err != nil {
		return err
	}
	p.mu.Lock()
	if sent.After(p.lastSent) {
		p.lastSent = sent
	}
	p.mu.Unlock()
	return nil
}

func (p *Presence) logWarn(format string, args ...any) {
	if p.logger != nil {
		p.logger.Warn(format, args...)
	}
}

func (p *Presence) logInfo(format string, args ...any) {
	if p.logger != nil {
		p.logger.Info(format, args...)
	}
}

// FleetMembers reads every live record of the registry, sorted by key.
//
// Any failure fails the whole read — the listing, or one record that cannot
// be read or decoded: the floor cannot evaluate a participant it cannot see,
// and a partial answer would let the readable ones speak for it.
func (c *Conn) FleetMembers(ctx context.Context) ([]queue.FleetMember, error) {
	if c == nil || c.presenceKV == nil || c.presenceKeys == nil {
		return nil, fmt.Errorf("queue/nats: fleet registry is not initialised")
	}
	// A push request carries no deadline of its own: the read is bounded
	// here, and a registry that does not answer in time is an error the
	// floor reports, never a wait the push sits in.
	ctx, cancel := context.WithTimeout(ctx, fleetReadTimeout)
	defer cancel()
	return readFleet(ctx, c.presenceKV, c.presenceKeys)
}

// fleetReadTimeout bounds one whole registry read (the listing and every
// record).
const fleetReadTimeout = 10 * time.Second

func readFleet(ctx context.Context, kv presenceKV, list presenceKeyLister) ([]queue.FleetMember, error) {
	keys, err := list(ctx)
	if err != nil {
		return nil, fmt.Errorf("queue/nats: list the fleet registry: %w", err)
	}
	sort.Strings(keys)
	members := make([]queue.FleetMember, 0, len(keys))
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue // expired or deregistered between the listing and the read
		}
		if err != nil {
			return nil, fmt.Errorf("queue/nats: read fleet record %s: %w", k, err)
		}
		var m queue.FleetMember
		if err := json.Unmarshal(entry.Value(), &m); err != nil {
			return nil, fmt.Errorf("queue/nats: decode fleet record %s: %w", k, err)
		}
		m.SeenAt = entry.Created()
		members = append(members, m)
	}
	return members, nil
}
