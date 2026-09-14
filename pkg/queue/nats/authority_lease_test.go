package nats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPortAuthorityRefreshLeaseFencesOwnersAndExpiresMonotonically(t *testing.T) {
	kv := &memoryEpochKV{}
	conn := &Conn{rolloutKV: kv}
	first := NewPortAuthorityRefreshLeaseSource(conn, "server-a")
	second := NewPortAuthorityRefreshLeaseSource(conn, "server-b")
	ctx := context.Background()
	lease, err := first.Acquire(ctx, 4, time.Now().UTC().Add(30*time.Second))
	if err != nil || lease.Token != 1 || lease.Owner != "server-a" {
		t.Fatalf("first lease = %+v, %v", lease, err)
	}
	if _, err := second.Acquire(ctx, 4, time.Now().UTC().Add(30*time.Second)); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("live lease did not fence competing owner: %v", err)
	}
	kv.mu.Lock()
	var record authorityRefreshLeaseRecord
	if err := json.Unmarshal(kv.value, &record); err != nil {
		kv.mu.Unlock()
		t.Fatal(err)
	}
	record.ExpiresAt = time.Now().UTC().Add(-time.Second)
	kv.value, _ = json.Marshal(record)
	kv.mu.Unlock()
	secondLease, err := second.Acquire(ctx, 4, time.Now().UTC().Add(30*time.Second))
	if err != nil || secondLease.Token != 2 || secondLease.Owner != "server-b" {
		t.Fatalf("expired lease was not replaced with the next fence: %+v, %v", secondLease, err)
	}
	if _, err := first.Acquire(ctx, 4, time.Now().UTC().Add(30*time.Second)); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("new owner did not fence previous owner: %v", err)
	}
}

func TestPortAuthorityRefreshLeaseRetriesKVRevisionConflict(t *testing.T) {
	kv := &memoryEpochKV{}
	conn := &Conn{rolloutKV: kv}
	source := NewPortAuthorityRefreshLeaseSource(conn, "server-a")
	if _, err := source.Acquire(context.Background(), 4, time.Now().UTC().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	kv.mu.Lock()
	var record authorityRefreshLeaseRecord
	if err := json.Unmarshal(kv.value, &record); err != nil {
		kv.mu.Unlock()
		t.Fatal(err)
	}
	record.ExpiresAt = time.Now().UTC().Add(-time.Second)
	kv.value, _ = json.Marshal(record)
	kv.conflictOnce = true
	kv.mu.Unlock()
	lease, err := source.Acquire(context.Background(), 4, time.Now().UTC().Add(30*time.Second))
	if err != nil || lease.Token != 2 {
		t.Fatalf("lease did not retry a KV CAS conflict: %+v, %v", lease, err)
	}
}
