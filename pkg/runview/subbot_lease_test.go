package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type childLeaseTestStore struct {
	store.RunStore
	lock    store.RunBirthReservation
	enabled bool
	calls   atomic.Int32
	delay   time.Duration
}

func (s *childLeaseTestStore) Capabilities() store.Capabilities {
	return store.Capabilities{CrossProcessLock: s.enabled}
}
func (s *childLeaseTestStore) BeginRunBirth(context.Context, string) (store.RunBirthReservation, error) {
	s.calls.Add(1)
	time.Sleep(s.delay)
	return s.lock, nil
}

type childRenewableTestLock struct {
	ttl      time.Duration
	refresh  func(context.Context) error
	releases atomic.Int32
}

func (l *childRenewableTestLock) Publish(context.Context, store.RunCreateSpec) (*store.Run, error) {
	return nil, store.ErrRunBirthUnavailable
}

func (l *childRenewableTestLock) Unlock() error                     { l.releases.Add(1); return nil }
func (l *childRenewableTestLock) Refresh(ctx context.Context) error { return l.refresh(ctx) }
func (l *childRenewableTestLock) LeaseTTL() time.Duration           { return l.ttl }

func childLeaseParent(t *testing.T) (context.Context, *store.FilesystemRunStore) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRun(t.Context(), "parent", "parent", nil); err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "parent", Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, s
}

func TestSubbotLeaseOwnsBeforeBirthAndReleasesBeforePark(t *testing.T) {
	ctx, s := childLeaseParent(t)
	parent, cancel := context.WithCancel(ctx)
	defer cancel()
	lease, err := AcquireSubbotLease(parent, s, "parent", "child", SubbotLeaseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if err = store.RequireRunValueFormat(lease.Context, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadRun(ctx, "child"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("lock created child row: %v", err)
	}
	childCtx, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "child", Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if rival, err := s.BeginRunBirth(childCtx, "child"); err == nil {
		_ = rival.Unlock()
		t.Fatal("pre-birth lock not held")
	}
	p, err := lease.PrepareInvocation(lease.Context, s, store.RunCreateSpec{ID: "child", WorkflowName: "child", Status: store.RunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	park, err := SubbotParkContext(parent, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	if lease.Context.Err() == nil || park.Err() != nil {
		t.Fatalf("lifetimes: active=%v park=%v", lease.Context.Err(), park.Err())
	}
	if err = store.RequireRunValueFormat(park, "child"); err != nil {
		t.Fatal(err)
	}
	rival, err := s.LockRun(park, "child")
	if err != nil {
		t.Fatal(err)
	}
	_ = rival.Unlock()
	cancel()
	if park.Err() == nil {
		t.Fatal("park lost parent cancellation")
	}
}

func TestSubbotLeaseRefusesBeforeLock(t *testing.T) {
	ctx, s := childLeaseParent(t)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		enabled bool
	}{
		{"missing binding", t.Context(), true}, {"no cross-process support", ctx, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &childLeaseTestStore{RunStore: s, enabled: tc.enabled}
			if _, err := AcquireSubbotLease(tc.ctx, wrapped, "parent", "child", SubbotLeaseOptions{}); err == nil {
				t.Fatal("accepted unsupported birth")
			}
			if wrapped.calls.Load() != 0 {
				t.Fatal("acquired before refusal")
			}
		})
	}
}

func TestSubbotLeaseRejectsInvalidCadence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ttl, interval time.Duration
	}{
		{"zero ttl", 0, 0}, {"truncated interval", time.Nanosecond, 0}, {"at ttl", time.Second, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s := childLeaseParent(t)
			lock := &childRenewableTestLock{ttl: tc.ttl, refresh: func(context.Context) error { t.Error("invalid lease refreshed"); return nil }}
			wrapped := &childLeaseTestStore{RunStore: s, lock: lock, enabled: true}
			if _, err := AcquireSubbotLease(ctx, wrapped, "parent", "child", SubbotLeaseOptions{HeartbeatInterval: tc.interval}); !errors.Is(err, ErrSubbotLivenessUnavailable) {
				t.Fatalf("error=%v", err)
			}
			if lock.releases.Load() != 1 {
				t.Fatal("refused lock leaked")
			}
		})
	}
}

func TestSubbotLeaseRefreshesDuringPreparation(t *testing.T) {
	ctx, s := childLeaseParent(t)
	refreshed := make(chan struct{}, 20)
	lock := &childRenewableTestLock{ttl: 500 * time.Millisecond, refresh: func(context.Context) error { refreshed <- struct{}{}; return nil }}
	wrapped := &childLeaseTestStore{RunStore: s, lock: lock, enabled: true}
	lease, err := AcquireSubbotLease(ctx, wrapped, "parent", "child", SubbotLeaseOptions{HeartbeatInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	// No child row/engine yet: preparation still owes lease renewal.
	for i := 0; i < 12; i++ {
		select {
		case <-refreshed:
		case <-lease.Context.Done():
			t.Fatalf("lease expired during preparation: %v", context.Cause(lease.Context))
		case <-time.After(2 * time.Second):
			t.Fatal("no refresh")
		}
	}
	if lease.Context.Err() != nil {
		t.Fatal(context.Cause(lease.Context))
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	if lock.releases.Load() != 1 {
		t.Fatal("release is not idempotent")
	}
}

func TestSubbotLeaseExpiresWhileRefreshBlocked(t *testing.T) {
	ctx, s := childLeaseParent(t)
	entered := make(chan struct{})
	returned := make(chan struct{})
	lock := &childRenewableTestLock{ttl: 200 * time.Millisecond, refresh: func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(returned)
		return nil /* late ACK must not revive */
	}}
	wrapped := &childLeaseTestStore{RunStore: s, lock: lock, enabled: true}
	lease, err := AcquireSubbotLease(ctx, wrapped, "parent", "child", SubbotLeaseOptions{HeartbeatInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never began")
	}
	select {
	case <-lease.Context.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("blocked RPC outlived acknowledged validity")
	}
	if !errors.Is(context.Cause(lease.Context), runtime.ErrRunInterrupted) || !errors.Is(context.Cause(lease.Context), ErrSubbotLeaseExpired) {
		t.Fatalf("cause=%v", context.Cause(lease.Context))
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("unlock did not join refresher")
	}
	if lock.releases.Load() != 1 {
		t.Fatal("lock not released exactly once")
	}
}

// Lease identity survives replacing only the child's value-format intention.
type childLeaseIdentityStore struct {
	store.RunStore
	got context.Context
}

func (s *childLeaseIdentityStore) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	s.got = ctx
	return store.AsRunBirthStore(s.RunStore).BeginRunBirth(ctx, id)
}
func TestSubbotLeasePreservesAdmittedIdentity(t *testing.T) {
	ctx, s := childLeaseParent(t)
	ctx = store.WithLeaseIdentity(ctx, "tenant-a", "pool-a")
	wrapped := &childLeaseIdentityStore{RunStore: s}
	lease, err := AcquireSubbotLease(ctx, wrapped, "parent", "child", SubbotLeaseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	ident, ok := store.LeaseIdentityFromContext(wrapped.got)
	if !ok || ident.TenantID != "tenant-a" || ident.Pool != "pool-a" {
		t.Fatalf("identity lost: %+v", ident)
	}
	if err = store.RequireRunValueFormat(wrapped.got, "child"); err != nil {
		t.Fatal(err)
	}
	if err = store.RequireRunValueFormat(lease.Context, "parent"); err != nil {
		t.Fatal(err)
	}
}

func TestSubbotLeaseRejectsExpiredAcquisition(t *testing.T) {
	ctx, s := childLeaseParent(t)
	lock := &childRenewableTestLock{ttl: 10 * time.Millisecond, refresh: func(context.Context) error { return nil }}
	wrapped := &childLeaseTestStore{RunStore: s, enabled: true, lock: lock, delay: 25 * time.Millisecond}
	lease, err := AcquireSubbotLease(ctx, wrapped, "parent", "child", SubbotLeaseOptions{})
	if lease != nil || !errors.Is(err, ErrSubbotLeaseExpired) || lock.releases.Load() != 1 {
		t.Fatalf("lease=%v err=%v unlocks=%d", lease, err, lock.releases.Load())
	}
}

func TestSubbotLeaseJoinsLateRefreshBeforeUnlock(t *testing.T) {
	ctx, s := childLeaseParent(t)
	entered, allow := make(chan struct{}), make(chan struct{})
	var releaseBarrier sync.Once
	defer releaseBarrier.Do(func() { close(allow) })
	lock := &childRenewableTestLock{ttl: 150 * time.Millisecond, refresh: func(context.Context) error { close(entered); <-allow; return nil }}
	wrapped := &childLeaseTestStore{RunStore: s, lock: lock, enabled: true}
	lease, err := AcquireSubbotLease(ctx, wrapped, "parent", "child", SubbotLeaseOptions{HeartbeatInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { releaseBarrier.Do(func() { close(allow) }); _ = lease.Release() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh not entered")
	}
	select {
	case <-lease.Context.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog blocked behind refresh")
	}
	done := make(chan error, 1)
	go func() { done <- lease.Release() }()
	select {
	case <-done:
		t.Fatal("release returned before refresh finished")
	case <-time.After(20 * time.Millisecond):
	}
	if lock.releases.Load() != 0 {
		t.Fatal("unlocked during refresh")
	}
	releaseBarrier.Do(func() { close(allow) })
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release did not join")
	}
	if lease.Context.Err() == nil || lock.releases.Load() != 1 {
		t.Fatal("late ACK revived lease or lost unlock")
	}
}

func TestSubbotReservationAbortLeavesNoPublishedShell(t *testing.T) {
	ctx, runs := childLeaseParent(t)
	parent, cancel := context.WithCancel(ctx)
	defer cancel()
	lease, err := AcquireSubbotLease(parent, runs, "parent", "child", SubbotLeaseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	// Manager refusal or cancellation can precede publication. Neither may
	// leave the legacy lock-only directory that makes liveness scans fail.
	for _, phase := range []string{"reserved", "released"} {
		if phase == "released" {
			cancel()
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(filepath.Join(runs.Root(), "runs", "child")); !os.IsNotExist(err) {
			t.Fatalf("%s published a shell: %v", phase, err)
		}
	}
	prepared, err := lease.PrepareInvocation(ctx, runs, store.RunCreateSpec{ID: "child", Status: store.RunStatusRunning})
	if err == nil || prepared.Context != nil {
		t.Fatalf("released reservation published: %+v %v", prepared, err)
	}
}
