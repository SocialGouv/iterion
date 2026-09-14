package mongo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func seedRefreshActivation(t *testing.T) (*Store, *store.PortActivation) {
	t.Helper()
	s := nativeNamespaceStore(t)
	identity, err := store.PortStoreIdentity(s)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := &store.PortActivation{
		Version: store.PortActivationVersion, Revision: 1, ProofRevision: 1, Enabled: true,
		Scope: store.PortActivationDistributed, StoreIdentity: identity,
		ProofDigest: strings.Repeat("a", 64), CapabilityDigest: portsactivation.CapabilityDigest(store.PortActivationDistributed),
		QueueVersion: queue.SchemaVersion, ConsumerAccessEvidence: "test observations, not production authority",
		VerifiedAt: now.Add(-30 * time.Second), ExpiresAt: now.Add(30 * time.Second),
	}
	if err := s.SavePortActivation(t.Context(), 0, a); err != nil {
		t.Fatal(err)
	}
	return s, a
}

func refreshLease(owner string, token uint64) store.PortActivationRefreshLease {
	return store.PortActivationRefreshLease{Owner: owner, Token: token, ExpiresAt: time.Now().UTC().Add(50 * time.Second).Truncate(time.Millisecond)}
}

func refreshRequest(a *store.PortActivation) store.PortActivationRenewal {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return store.PortActivationRenewal{PolicyRevision: a.Revision, ProofRevision: a.ProofRevision,
		ProofDigest: a.ProofDigest, Lease: *a.RefreshLease, VerifiedAt: now, ExpiresAt: now.Add(time.Minute)}
}

func TestNativeActivationMongoRenewalPreservesPolicy(t *testing.T) {
	s, initial := seedRefreshActivation(t)
	ctx := t.Context()
	claimed, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-a", 10))
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.ExpiresAt.Equal(initial.ExpiresAt) || claimed.ProofRevision != initial.ProofRevision {
		t.Fatal("claiming authority ownership extended the authorization")
	}
	for range 8 {
		next, err := s.RenewPortActivation(ctx, refreshRequest(claimed))
		if err != nil {
			t.Fatal(err)
		}
		if next.Revision != initial.Revision || next.ProofRevision != claimed.ProofRevision+1 ||
			next.ProofDigest != initial.ProofDigest || next.CapabilityDigest != initial.CapabilityDigest ||
			next.StoreIdentity != initial.StoreIdentity || !next.Enabled || !next.ExpiresAt.After(initial.ExpiresAt) {
			t.Fatalf("renewal changed policy or lost freshness: %+v", next)
		}
		claimed = next
	}
	// A renewable record is still not an authority. Raw Mongo must continue
	// refusing launch until the production structured-proof verifier is wired.
	if err := store.RequirePortActivation(ctx, s, store.PortActivationDistributed, time.Now()); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("fresh hand-written record granted admission: %v", err)
	}
}

func TestNativeActivationMongoRefresherFencing(t *testing.T) {
	s, initial := seedRefreshActivation(t)
	ctx := t.Context()
	first, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-a", 10))
	if err != nil {
		t.Fatal(err)
	}
	stale := refreshRequest(first)
	second, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-b", 20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewPortActivation(ctx, stale); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("old lease owner renewed after successor: %v", err)
	}
	for _, token := range []uint64{9, 10, 20} {
		if _, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-a", token)); !errors.Is(err, store.ErrRunConflict) {
			t.Fatalf("non-increasing fencing token %d was accepted: %v", token, err)
		}
	}
	request := refreshRequest(second)
	if _, err := s.RenewPortActivation(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewPortActivation(ctx, request); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("duplicate proof revision accepted: %v", err)
	}
}

func TestNativeActivationMongoDisableWinsRenewal(t *testing.T) {
	s, initial := seedRefreshActivation(t)
	ctx := t.Context()
	current, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-a", 10))
	if err != nil {
		t.Fatal(err)
	}
	// Complete one real refresh, then keep writing while Disable races it.
	current, err = s.RenewPortActivation(ctx, refreshRequest(current))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 1)
	var worker sync.WaitGroup
	worker.Go(func() {
		<-start
		for range 100 {
			next, err := s.RenewPortActivation(ctx, refreshRequest(current))
			if errors.Is(err, store.ErrRunConflict) {
				return
			}
			if err != nil {
				errorsCh <- err
				return
			}
			current = next
		}
	})
	close(start)
	disabled, disableErr := s.DisablePortActivation(ctx, initial.Revision)
	worker.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if disableErr != nil {
		t.Fatalf("freshness revisions blocked Disable: %v", disableErr)
	}
	if disabled.Enabled || disabled.Revision != initial.Revision+1 || disabled.ProofRevision < 2 || disabled.RefreshLease != nil {
		t.Fatalf("Disable lost policy or latest proof: %+v", disabled)
	}
	if _, err := s.ClaimPortActivationRefresh(ctx, disabled.Revision, refreshLease("server-b", 99)); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("refresher re-enabled disabled policy: %v", err)
	}
	if _, err := s.RenewPortActivation(ctx, refreshRequest(current)); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("stale refresh restored disabled admission: %v", err)
	}
	loaded, err := s.LoadPortActivation(ctx)
	if err != nil || loaded.Enabled || loaded.Revision != disabled.Revision || loaded.ProofRevision != disabled.ProofRevision {
		t.Fatalf("post-disable state changed: %+v, %v", loaded, err)
	}
	// A policy action, in contrast to refresh traffic, invalidates an older
	// operator request. It cannot disable a later explicit activation.
	if _, err := s.DisablePortActivation(ctx, initial.Revision); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("superseded operator request accepted: %v", err)
	}
}

func TestNativeActivationMongoRenewalRejectsChangedEvidence(t *testing.T) {
	s, initial := seedRefreshActivation(t)
	ctx := t.Context()
	claimed, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-a", 10))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*store.PortActivationRenewal){
		"fingerprint":    func(r *store.PortActivationRenewal) { r.ProofDigest = strings.Repeat("b", 64) },
		"owner":          func(r *store.PortActivationRenewal) { r.Lease.Owner = "server-b" },
		"policy":         func(r *store.PortActivationRenewal) { r.PolicyRevision++ },
		"proof":          func(r *store.PortActivationRenewal) { r.ProofRevision++ },
		"extended lease": func(r *store.PortActivationRenewal) { r.Lease.ExpiresAt = r.Lease.ExpiresAt.Add(time.Second) },
		"past observation": func(r *store.PortActivationRenewal) {
			r.VerifiedAt = initial.VerifiedAt.Add(-time.Second)
			r.ExpiresAt = r.VerifiedAt.Add(time.Minute)
		},
		"future observation": func(r *store.PortActivationRenewal) { r.VerifiedAt = r.VerifiedAt.Add(time.Second) },
		"overlong proof":     func(r *store.PortActivationRenewal) { r.ExpiresAt = r.VerifiedAt.Add(time.Minute + time.Second) },
		"expired lease":      func(r *store.PortActivationRenewal) { r.Lease.ExpiresAt = time.Now().Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			r := refreshRequest(claimed)
			edit(&r)
			if _, err := s.RenewPortActivation(ctx, r); !errors.Is(err, store.ErrRunConflict) && !errors.Is(err, store.ErrPortActivation) {
				t.Fatalf("changed renewal accepted: %v", err)
			}
		})
	}
	loaded, err := s.LoadPortActivation(ctx)
	if err != nil || loaded.ProofRevision != initial.ProofRevision || !loaded.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("refusal mutated freshness: %+v, %v", loaded, err)
	}
}

func TestNativeActivationMongoRefusesOtherSchema(t *testing.T) {
	for _, version := range []int{store.PortActivationVersion - 1, store.PortActivationVersion + 1} {
		s, initial := seedRefreshActivation(t)
		ctx := context.Background()
		if _, err := s.portActivationCollection().UpdateOne(ctx, bson.M{"_id": "current"}, bson.M{"$set": bson.M{"version": version}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadPortActivation(ctx); !errors.Is(err, store.ErrPortActivation) {
			t.Fatalf("schema %d loaded: %v", version, err)
		}
		if _, err := s.DisablePortActivation(ctx, initial.Revision); !errors.Is(err, store.ErrRunConflict) {
			t.Fatalf("schema %d was rewritten by Disable: %v", version, err)
		}
		if _, err := s.ClaimPortActivationRefresh(ctx, initial.Revision, refreshLease("server-a", 10)); !errors.Is(err, store.ErrRunConflict) {
			t.Fatalf("schema %d was rewritten by refresher: %v", version, err)
		}
		next := *initial
		next.Revision++
		if err := s.SavePortActivation(ctx, initial.Revision, &next); !errors.Is(err, store.ErrRunConflict) {
			t.Fatalf("schema %d was silently migrated: %v", version, err)
		}
	}
}
