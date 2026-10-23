package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// errStoreUnreachable is shaped like the error a real Mongo driver returns
// when it cannot select a server: it names an internal host, which no
// client may ever be shown.
var errStoreUnreachable = errors.New("server selection error: context deadline exceeded, current topology: { Type: Unknown, Servers: [{ Addr: iterion-mongo-0.iterion-mongo-headless.iterion-prod.svc.cluster.local:27017, Type: Unknown }] }")

// nthCallGate is an identity store whose reads fail on chosen calls, the way
// a saturated pool or a store restart fails ONE read of a request, not
// necessarily its first. failOn maps a method name to the 1-based call
// numbers that fail; call 0 fails every call of that method. Each failure
// wraps err, so a test can choose a cancellation-shaped error
// (context.Canceled) or a driver-shaped one (errStoreUnreachable).
type nthCallGate struct {
	identity.Store
	err error

	mu     sync.Mutex
	failOn map[string]map[int]bool
	calls  map[string]int
}

func newNthCallGate(st identity.Store, err error) *nthCallGate {
	return &nthCallGate{Store: st, err: err, failOn: map[string]map[int]bool{}, calls: map[string]int{}}
}

// fail arms the gate: the given calls of method fail (0 = every call).
func (g *nthCallGate) fail(method string, calls ...int) *nthCallGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failOn[method] == nil {
		g.failOn[method] = map[int]bool{}
	}
	for _, c := range calls {
		g.failOn[method][c] = true
	}
	return g
}

// disarm stops every failure; call counts keep running.
func (g *nthCallGate) disarm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failOn = map[string]map[int]bool{}
}

func (g *nthCallGate) count(method string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[method]
}

func (g *nthCallGate) hit(method string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls[method]++
	n := g.calls[method]
	if f := g.failOn[method]; f != nil && (f[0] || f[n]) {
		return fmt.Errorf("identity: %s: %w", method, g.err)
	}
	return nil
}

func (g *nthCallGate) GetUser(ctx context.Context, id string) (identity.User, error) {
	if err := g.hit("GetUser"); err != nil {
		return identity.User{}, err
	}
	return g.Store.GetUser(ctx, id)
}

func (g *nthCallGate) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	if err := g.hit("GetTeam"); err != nil {
		return identity.Team{}, err
	}
	return g.Store.GetTeam(ctx, id)
}

func (g *nthCallGate) GetOrg(ctx context.Context, id string) (identity.Org, error) {
	if err := g.hit("GetOrg"); err != nil {
		return identity.Org{}, err
	}
	return g.Store.GetOrg(ctx, id)
}

func (g *nthCallGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if err := g.hit("GetMembership"); err != nil {
		return identity.Membership{}, err
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

func (g *nthCallGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if err := g.hit("GetOrgMembership"); err != nil {
		return identity.OrgMembership{}, err
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

func (g *nthCallGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if err := g.hit("ListMembershipsByUser"); err != nil {
		return nil, err
	}
	return g.Store.ListMembershipsByUser(ctx, userID)
}

func (g *nthCallGate) ListOrgMembershipsByUser(ctx context.Context, userID string) ([]identity.OrgMembership, error) {
	if err := g.hit("ListOrgMembershipsByUser"); err != nil {
		return nil, err
	}
	return g.Store.ListOrgMembershipsByUser(ctx, userID)
}

func (g *nthCallGate) ListTeamsByOrg(ctx context.Context, orgID string) ([]identity.Team, error) {
	if err := g.hit("ListTeamsByOrg"); err != nil {
		return nil, err
	}
	return g.Store.ListTeamsByOrg(ctx, orgID)
}

// gatedSessions is the in-memory session store with a per-operation latency
// (what makes two refreshes of one token overlap), a one-shot failure of
// CreateSession (a store blip landing after the rotation), of the
// successor's check (SessionRevoked) and of its claim (ClaimSession), and the
// deadline every call carried.
type gatedSessions struct {
	*MemorySessionStore
	latency         atomic.Int64
	failCreateArmed atomic.Bool
	// landThenFailCreateArmed writes the next CreateSession, then answers
	// an error: a write whose answer was lost.
	landThenFailCreateArmed atomic.Bool
	failCheckArmed          atomic.Bool
	// passGate, when set (holdPasses), holds every revocation pass.
	passGate chan struct{}
	// landThenFailClaimArmed claims on the next ClaimSession, then answers
	// an error: a claim whose answer was lost.
	landThenFailClaimArmed atomic.Bool
	// failRevokeAllArmed and failRevokeLineageArmed fail the next
	// revocation of an account or of a lineage, before it records anything.
	failRevokeAllArmed     atomic.Bool
	failRevokeLineageArmed atomic.Bool
	// failGeneration fails every SessionGeneration while set.
	failGeneration atomic.Bool
	// failGetArmed fails the next GetSessionByTokenHash.
	failGetArmed atomic.Bool
	creates      atomic.Int64
	claims       atomic.Int64

	// beforeCheck, when set, runs once at the start of the next
	// SessionRevoked: another actor landing between a successor's write
	// and its check.
	beforeCheck func()
	// deadlines records, per method, whether each call carried a deadline
	// and how far ahead of the call it was (zero without one).
	deadlines map[string][]time.Duration

	// preempt, when set, runs once inside the next CreateSession, before
	// it: whatever row it returns is written first under the same token
	// hash — another writer landing between a read and this write.
	mu2     sync.Mutex
	preempt func(next Session) Session
	// beforeCreate, when set, runs once at the start of the next
	// CreateSession: another actor (a sign-out, a revoke-all) landing between
	// a refresh's rotation and its successor's write.
	beforeCreate func()
	// afterCreate, when set, runs once after the next CreateSession landed
	// and before it answers: another presentation reading the row while the
	// writer still waits for its write's answer.
	afterCreate func()
	// beforeClaim, when set, runs once at the start of the next
	// ClaimSession: another actor landing between a successor's check and
	// its claim.
	beforeClaim func()
}

// runAfterNextCreate arms afterCreate for the next CreateSession.
func (s *gatedSessions) runAfterNextCreate(f func()) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.afterCreate = f
}

// runBeforeNextClaim arms beforeClaim for the next ClaimSession.
func (s *gatedSessions) runBeforeNextClaim(f func()) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.beforeClaim = f
}

// takeHook returns *hook and clears it, under the gate's lock.
func (s *gatedSessions) takeHook(hook *func()) func() {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	f := *hook
	*hook = nil
	return f
}

// preemptNextCreate arms preempt for the next CreateSession.
func (s *gatedSessions) preemptNextCreate(f func(next Session) Session) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.preempt = f
}

// runBeforeNextCreate arms beforeCreate for the next CreateSession.
func (s *gatedSessions) runBeforeNextCreate(f func()) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.beforeCreate = f
}

// runBeforeNextCheck arms beforeCheck for the next SessionRevoked.
func (s *gatedSessions) runBeforeNextCheck(f func()) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.beforeCheck = f
}

// record notes the deadline ctx carries for a call of method.
func (s *gatedSessions) record(ctx context.Context, method string) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	if s.deadlines == nil {
		s.deadlines = map[string][]time.Duration{}
	}
	var ahead time.Duration
	if d, ok := ctx.Deadline(); ok {
		ahead = time.Until(d)
	}
	s.deadlines[method] = append(s.deadlines[method], ahead)
}

// resetDeadlines forgets the deadlines recorded so far.
func (s *gatedSessions) resetDeadlines() {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.deadlines = nil
}

// recordedDeadlines returns the deadlines recorded so far, per method.
func (s *gatedSessions) recordedDeadlines() map[string][]time.Duration {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	out := make(map[string][]time.Duration, len(s.deadlines))
	for k, v := range s.deadlines {
		out[k] = append([]time.Duration(nil), v...)
	}
	return out
}

func newGatedSessions() *gatedSessions {
	return &gatedSessions{MemorySessionStore: NewMemorySessionStore()}
}

func (s *gatedSessions) sleep() {
	if d := time.Duration(s.latency.Load()); d > 0 {
		time.Sleep(d)
	}
}

func (s *gatedSessions) CreateSession(ctx context.Context, sess Session) error {
	s.record(ctx, "CreateSession")
	s.sleep()
	s.mu2.Lock()
	before := s.beforeCreate
	s.beforeCreate = nil
	s.mu2.Unlock()
	if before != nil {
		before()
	}
	s.creates.Add(1)
	if s.failCreateArmed.CompareAndSwap(true, false) {
		return fmt.Errorf("auth: insert session: %w", errStoreUnreachable)
	}
	if s.landThenFailCreateArmed.CompareAndSwap(true, false) {
		if err := s.MemorySessionStore.CreateSession(ctx, sess); err != nil {
			return err
		}
		if after := s.takeHook(&s.afterCreate); after != nil {
			after()
		}
		return fmt.Errorf("auth: insert session: %w", errStoreUnreachable)
	}
	s.mu2.Lock()
	pre := s.preempt
	s.preempt = nil
	s.mu2.Unlock()
	if pre != nil {
		if err := s.MemorySessionStore.CreateSession(ctx, pre(sess)); err != nil {
			return fmt.Errorf("preempting write: %w", err)
		}
	}
	if err := s.MemorySessionStore.CreateSession(ctx, sess); err != nil {
		return err
	}
	if after := s.takeHook(&s.afterCreate); after != nil {
		after()
	}
	return nil
}

func (s *gatedSessions) GetSessionByTokenHash(ctx context.Context, h string) (Session, error) {
	s.record(ctx, "GetSessionByTokenHash")
	s.sleep()
	if s.failGetArmed.CompareAndSwap(true, false) {
		return Session{}, fmt.Errorf("auth: get session: %w", errStoreUnreachable)
	}
	return s.MemorySessionStore.GetSessionByTokenHash(ctx, h)
}

func (s *gatedSessions) RevokeSessionIfNotRevoked(ctx context.Context, id string, at time.Time) (bool, error) {
	s.record(ctx, "RevokeSessionIfNotRevoked")
	s.sleep()
	return s.MemorySessionStore.RevokeSessionIfNotRevoked(ctx, id, at)
}

func (s *gatedSessions) RotateSession(ctx context.Context, id string, rot Rotation) (time.Time, bool, error) {
	s.record(ctx, "RotateSession")
	s.sleep()
	return s.MemorySessionStore.RotateSession(ctx, id, rot)
}

func (s *gatedSessions) RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (int64, error) {
	s.record(ctx, "RecordAccountRevocation")
	s.sleep()
	if s.failRevokeAllArmed.CompareAndSwap(true, false) {
		return 0, fmt.Errorf("auth: revoke user sessions: raise the session generation: %w", errStoreUnreachable)
	}
	return s.MemorySessionStore.RecordAccountRevocation(ctx, userID, at)
}

func (s *gatedSessions) RecordLineageRevocation(ctx context.Context, userID, lineageID string, at time.Time) error {
	s.record(ctx, "RecordLineageRevocation")
	s.sleep()
	if s.failRevokeLineageArmed.CompareAndSwap(true, false) {
		return fmt.Errorf("auth: revoke lineage %s: record the revocation: %w", lineageID, errStoreUnreachable)
	}
	return s.MemorySessionStore.RecordLineageRevocation(ctx, userID, lineageID, at)
}

func (s *gatedSessions) RevokeAccountSessions(ctx context.Context, userID string, below int64, at time.Time) (int64, error) {
	s.record(ctx, "RevokeAccountSessions")
	s.sleep()
	s.waitPassGate()
	return s.MemorySessionStore.RevokeAccountSessions(ctx, userID, below, at)
}

func (s *gatedSessions) RevokeLineageSessions(ctx context.Context, userID, lineageID string, at time.Time) (int64, error) {
	s.record(ctx, "RevokeLineageSessions")
	s.sleep()
	s.waitPassGate()
	return s.MemorySessionStore.RevokeLineageSessions(ctx, userID, lineageID, at)
}

// holdPasses holds every revocation pass until the returned release runs: a
// revocation's record lands, its pass does not.
func (s *gatedSessions) holdPasses() (release func()) {
	gate := make(chan struct{})
	s.mu2.Lock()
	s.passGate = gate
	s.mu2.Unlock()
	return func() {
		s.mu2.Lock()
		s.passGate = nil
		s.mu2.Unlock()
		close(gate)
	}
}

// waitPassGate blocks while holdPasses holds the passes.
func (s *gatedSessions) waitPassGate() {
	s.mu2.Lock()
	gate := s.passGate
	s.mu2.Unlock()
	if gate != nil {
		<-gate
	}
}

func (s *gatedSessions) SessionGeneration(ctx context.Context, userID string) (int64, error) {
	s.record(ctx, "SessionGeneration")
	if s.failGeneration.Load() {
		return 0, fmt.Errorf("auth: read the session generation: %w", errStoreUnreachable)
	}
	return s.MemorySessionStore.SessionGeneration(ctx, userID)
}

func (s *gatedSessions) RevokeSession(ctx context.Context, id string, at time.Time) (Rotation, error) {
	s.record(ctx, "RevokeSession")
	s.sleep()
	return s.MemorySessionStore.RevokeSession(ctx, id, at)
}

func (s *gatedSessions) SessionRevoked(ctx context.Context, sess Session) (bool, error) {
	s.record(ctx, "SessionRevoked")
	s.sleep()
	s.mu2.Lock()
	before := s.beforeCheck
	s.beforeCheck = nil
	s.mu2.Unlock()
	if before != nil {
		before()
	}
	// Like the driver, a read whose context ended answers that end.
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("auth: read the revocations of session %s: %w", sess.ID, err)
	}
	if s.failCheckArmed.CompareAndSwap(true, false) {
		return false, fmt.Errorf("auth: read the revocations of session %s: %w", sess.ID, errStoreUnreachable)
	}
	return s.MemorySessionStore.SessionRevoked(ctx, sess)
}

func (s *gatedSessions) ClaimSession(ctx context.Context, id string) (bool, error) {
	s.record(ctx, "ClaimSession")
	s.sleep()
	// Like the driver, a write whose context ended is not sent.
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("auth: claim session %s: %w", id, err)
	}
	if before := s.takeHook(&s.beforeClaim); before != nil {
		before()
	}
	s.claims.Add(1)
	if s.landThenFailClaimArmed.CompareAndSwap(true, false) {
		if _, err := s.MemorySessionStore.ClaimSession(ctx, id); err != nil {
			return false, err
		}
		return false, fmt.Errorf("auth: claim session %s: %w", id, errStoreUnreachable)
	}
	return s.MemorySessionStore.ClaimSession(ctx, id)
}

func (s *gatedSessions) Now(ctx context.Context) (time.Time, error) {
	s.record(ctx, "Now")
	return s.MemorySessionStore.Now(ctx)
}

// testClock is the service clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// rotationFixture is a Service on gated in-memory stores with a hand-moved
// clock, and an account holding two live sessions: the one a test presents
// (token) and another device's (other), whose survival tells a test whether
// reuse detection revoked every session of the account.
type rotationFixture struct {
	svc      *Service
	ids      *nthCallGate
	sessions *gatedSessions
	clock    *testClock
	userID   string
	token    string
	other    string
}

func newRotationFixture(t *testing.T, gateErr error) *rotationFixture {
	t.Helper()
	ctx := context.Background()
	f := &rotationFixture{
		ids:      newNthCallGate(identity.NewMemoryStore(), gateErr),
		sessions: newGatedSessions(),
		clock:    &testClock{now: time.Now().UTC()},
	}
	f.svc = newTestServiceOn(t, f.ids, f.sessions)
	f.svc.now = f.clock.Now
	// One clock for the service and the store: the window is measured on
	// the store's (SessionStore.Now), the expiries on the service's.
	f.sessions.SetClock(f.clock.Now)
	reg, err := f.svc.Register(ctx, "rotation@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	other, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "other-device", "ip2")
	if err != nil {
		t.Fatalf("Login (other device): %v", err)
	}
	f.userID, f.token, f.other = reg.User.ID, reg.RefreshToken, other.RefreshToken
	return f
}

// session reads the stored row of a raw refresh token, once every detached
// revocation pass started so far has written its rows.
func (f *rotationFixture) session(t *testing.T, raw string) Session {
	t.Helper()
	f.svc.WaitRevocationPasses()
	s, err := f.sessions.MemorySessionStore.GetSessionByTokenHash(context.Background(), HashRefreshToken(raw))
	if err != nil {
		t.Fatalf("read the session of a token: %v", err)
	}
	return s
}

// otherDeviceRevoked reports whether the other device's session — another
// lineage of the account, never presented by a test — was revoked: by a
// revocation of the whole account, never by one of the token's lineage.
func (f *rotationFixture) otherDeviceRevoked(t *testing.T) bool {
	t.Helper()
	return f.session(t, f.other).RevokedAt != nil
}

// successorsOf lists the sessions written as successors of raw's session.
func successorsOf(t *testing.T, f *rotationFixture, raw string) []Session {
	t.Helper()
	prev := f.session(t, raw)
	var out []Session
	for _, s := range sessionsOf(t, f.sessions.MemorySessionStore, f.userID) {
		if s.RotatedFromID == prev.ID {
			out = append(out, s)
		}
	}
	return out
}

// liveInLineage counts the sessions of the lineage raw's session belongs to
// that nothing revoked.
func (f *rotationFixture) liveInLineage(t *testing.T, raw string) int {
	t.Helper()
	lineage := f.session(t, raw).lineage()
	n := 0
	for _, s := range sessionsOf(t, f.sessions.MemorySessionStore, f.userID) {
		if s.lineage() == lineage && s.RevokedAt == nil {
			n++
		}
	}
	return n
}

// newTestSigner is a JWT signer on a fresh random key.
func newTestSigner(t *testing.T) *JWTSigner {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// newTestServiceOn is newTestService on the given stores.
func newTestServiceOn(t *testing.T, st identity.Store, sessions SessionStore) *Service {
	t.Helper()
	svc, err := NewService(Config{
		Store:      st,
		Sessions:   sessions,
		Signer:     newTestSigner(t),
		SignupMode: SignupOpen,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}
