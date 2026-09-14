package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/nats-io/nats.go/jetstream"
)

// PortAuthorityRefreshKey is the single fenced lease used by the trusted
// server authority refresher. It lives in the existing rollout bucket, under
// its own prefix, so the run-lock and rollout high-water keys keep their
// existing semantics and old EnsureSchema callers leave it untouched.
const PortAuthorityRefreshKey = "ports-v1.authority.refresh"

const authorityRefreshLeaseMaxAge = store.PortDistributedProofMaxAge

type PortAuthorityRefreshLeaseSource struct {
	conn  *Conn
	owner string
}

// NewPortAuthorityRefreshLeaseSource returns a distributed fencing source for
// the authority refresher. The owner is diagnostic identity only; the KV
// sequence token is the authoritative fence carried into Mongo CAS.
func NewPortAuthorityRefreshLeaseSource(conn *Conn, owner string) PortAuthorityRefreshLeaseSource {
	return PortAuthorityRefreshLeaseSource{conn: conn, owner: owner}
}

type authorityRefreshLeaseRecord struct {
	PolicyRevision uint64    `json:"policy_revision"`
	Owner          string    `json:"owner"`
	Token          uint64    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (r authorityRefreshLeaseRecord) validate() error {
	if r.PolicyRevision == 0 || r.Token == 0 || r.Token > math.MaxInt64 || r.Owner == "" || r.ExpiresAt.IsZero() {
		return fmt.Errorf("queue/nats: malformed authority refresh lease")
	}
	return nil
}

func (s PortAuthorityRefreshLeaseSource) Acquire(ctx context.Context, policy uint64, expiresAt time.Time) (store.PortActivationRefreshLease, error) {
	now := time.Now().UTC()
	if s.conn == nil || s.conn.rolloutKV == nil || s.owner == "" || policy == 0 ||
		expiresAt.IsZero() || !now.Before(expiresAt) || expiresAt.After(now.Add(authorityRefreshLeaseMaxAge)) {
		return store.PortActivationRefreshLease{}, fmt.Errorf("queue/nats: invalid authority refresh lease request")
	}
	kv := s.conn.rolloutKV
	for {
		if err := ctx.Err(); err != nil {
			return store.PortActivationRefreshLease{}, err
		}
		entry, err := kv.Get(ctx, PortAuthorityRefreshKey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			record := authorityRefreshLeaseRecord{PolicyRevision: policy, Owner: s.owner, Token: 1, ExpiresAt: expiresAt}
			if err := record.validate(); err != nil {
				return store.PortActivationRefreshLease{}, err
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return store.PortActivationRefreshLease{}, err
			}
			if _, err := kv.Create(ctx, PortAuthorityRefreshKey, raw); err == nil {
				return store.PortActivationRefreshLease{Owner: record.Owner, Token: record.Token, ExpiresAt: record.ExpiresAt}, nil
			} else if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			} else {
				return store.PortActivationRefreshLease{}, fmt.Errorf("queue/nats: create authority refresh lease: %w", err)
			}
		case err != nil:
			return store.PortActivationRefreshLease{}, fmt.Errorf("queue/nats: read authority refresh lease: %w", err)
		default:
			var previous authorityRefreshLeaseRecord
			decoder := json.NewDecoder(bytes.NewReader(entry.Value()))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&previous) != nil || decoder.Decode(new(any)) != io.EOF || previous.validate() != nil {
				return store.PortActivationRefreshLease{}, fmt.Errorf("queue/nats: stored authority refresh lease is malformed")
			}
			if now.Before(previous.ExpiresAt) {
				return store.PortActivationRefreshLease{}, fmt.Errorf("%w: authority refresh lease is owned by %s", store.ErrRunConflict, previous.Owner)
			}
			if previous.Token >= math.MaxInt64 {
				return store.PortActivationRefreshLease{}, fmt.Errorf("queue/nats: authority refresh fence exhausted")
			}
			next := authorityRefreshLeaseRecord{PolicyRevision: policy, Owner: s.owner, Token: previous.Token + 1, ExpiresAt: expiresAt}
			raw, err := json.Marshal(next)
			if err != nil {
				return store.PortActivationRefreshLease{}, err
			}
			if _, err := kv.Update(ctx, PortAuthorityRefreshKey, raw, entry.Revision()); err == nil {
				return store.PortActivationRefreshLease{Owner: next.Owner, Token: next.Token, ExpiresAt: next.ExpiresAt}, nil
			} else if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			} else {
				return store.PortActivationRefreshLease{}, fmt.Errorf("queue/nats: update authority refresh lease: %w", err)
			}
		}
	}
}

// AuthorityRefreshLeaseOwner returns a bounded diagnostic owner identity.
// It is useful for wiring a stable per-process value without ever deriving it
// from a credential-bearing URL.
func AuthorityRefreshLeaseOwner(hostname string, pid int) string {
	owner := strings.TrimSpace(hostname)
	if owner == "" {
		owner = "server"
	}
	if pid <= 0 {
		return owner
	}
	return fmt.Sprintf("%s-%d", owner, pid)
}
