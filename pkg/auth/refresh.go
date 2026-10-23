package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/lease"
)

// Session is a stored refresh token. The plaintext token is never
// persisted — only its SHA-256 hash. On rotation, the previous
// session is marked Revoked and a new one is created.
//
// RevokedAt is written once: by the rotation that retires the session, or by
// the sign-out or revoke-all that revokes it, whichever lands first. Nothing
// moves it afterwards — the reuse window is measured from it.
//
// A rotation records itself on the session it retires, in the same
// compare-and-set as RevokedAt (SessionStore.RotateSession): RotationNonce and
// RotationKeyID name the nonce and the server key the successor token was
// derived under (Service.Refresh); SuccessorHash is the successor's token
// hash, which pins the one successor that rotation minted; RotationUserAgent
// and RotationIP name the client the rotation was for. A sign-out and a
// revoke-all record themselves before anything else (SessionStore): from then
// on the successor that record names is refused at its delivery
// (Service.confirmSuccessor), whether or not their pass reached the session.
// The successor carries RotatedFromID, the id of the session it replaced.
//
// Every session belongs to a lineage: the sessions one login minted, its
// root, and every successor rotated from it. LineageID is the root's id —
// IssueSession sets it to the root's own id, every successor copies it — so a
// lineage is revoked as one family without walking RotatedFromID
// (SessionStore.RevokeLineage). Generation is the account's session
// generation the lineage was born under (SessionStore.SessionGeneration): a
// revoke-all raises the account's generation, and a session of a lower one is
// covered by it (SessionStore.SessionRevoked). A row without LineageID was
// last written before lineages were recorded: it is its own root
// (Session.lineage), and nothing links it to the successor its rotation
// wrote. Every write of a SessionStore stamps LineageID where it is absent.
//
// DeliveredAt is the instant the session's token was first handed out: a
// root's at its login (IssueSession), a successor's when a refresh claims it
// (SessionStore.ClaimSession) — the last step before that refresh answers.
// A successor written but never claimed was handed to nobody, so its
// predecessor is still its lineage's only credential (Service.refreshRetired).
type Session struct {
	ID                string     `bson:"_id" json:"id"`
	UserID            string     `bson:"user_id" json:"user_id"`
	TokenHash         string     `bson:"token_hash" json:"-"`
	UserAgent         string     `bson:"user_agent,omitempty" json:"user_agent,omitempty"`
	IP                string     `bson:"ip,omitempty" json:"ip,omitempty"`
	IssuedAt          time.Time  `bson:"issued_at" json:"issued_at"`
	ExpiresAt         time.Time  `bson:"expires_at" json:"expires_at"`
	RevokedAt         *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	DeliveredAt       *time.Time `bson:"delivered_at,omitempty" json:"-"`
	RotatedFromID     string     `bson:"rotated_from,omitempty" json:"-"`
	LineageID         string     `bson:"lineage,omitempty" json:"-"`
	Generation        int64      `bson:"generation,omitempty" json:"-"`
	RotationNonce     string     `bson:"rotation_nonce,omitempty" json:"-"`
	RotationKeyID     string     `bson:"rotation_kid,omitempty" json:"-"`
	SuccessorHash     string     `bson:"successor_hash,omitempty" json:"-"`
	RotationUserAgent string     `bson:"rotation_ua,omitempty" json:"-"`
	RotationIP        string     `bson:"rotation_ip,omitempty" json:"-"`
}

// lineage is the id of the lineage s belongs to: its LineageID, or its own
// id for a row written before lineages were recorded.
func (s Session) lineage() string {
	if s.LineageID != "" {
		return s.LineageID
	}
	return s.ID
}

// root names the lineage s belongs to: its lineage id and the account
// session generation it was born under.
func (s Session) root() SessionRoot {
	return SessionRoot{ID: s.lineage(), Generation: s.Generation}
}

// rotation is the rotation record s carries (zero when none).
func (s Session) rotation() Rotation {
	return Rotation{
		Nonce:         s.RotationNonce,
		KeyID:         s.RotationKeyID,
		SuccessorHash: s.SuccessorHash,
		UserAgent:     s.RotationUserAgent,
		IP:            s.RotationIP,
	}
}

// clearRotation drops the rotation record of s.
func (s *Session) clearRotation() {
	s.RotationNonce, s.RotationKeyID, s.SuccessorHash = "", "", ""
	s.RotationUserAgent, s.RotationIP = "", ""
}

// rotationRecordFields are the stored keys of the rotation record.
var rotationRecordFields = []string{"rotation_nonce", "rotation_kid", "successor_hash", "rotation_ua", "rotation_ip"}

// DefaultRefreshReuseGrace is the reuse grace window a Service applies when
// its Config names none.
const DefaultRefreshReuseGrace = 30 * time.Second

// SessionStore is the persistence interface for refresh tokens.
//
// A revocation of a lineage or of an account runs in two steps, in this
// order: it records itself (a watermark: the lineage marked revoked, the
// account's generation raised), then a pass revokes the sessions it covers
// that can still answer something. The record is the revocation's commit
// point: once it landed, every session it covers is refused at its next
// refresh and its access tokens at their next derivation, because every
// delivery of a successor reads the records after the successor's write
// (SessionRevoked; Service.confirmSuccessor) and every derivation reads them
// too (Service.CheckDerivation) — no interleaving with a rotation leaves a
// delivered session of a revoked family live. The pass is housekeeping the
// record does not depend on: it makes the live rows read as revoked, it is
// idempotent, and a pass that fails or never runs changes no answer. A
// record is never reaped: it is what covers a session its pass did not
// reach, for as long as that session lives.
//
// No session row is ever deleted by a refresh: a row a refresh wrote but
// could not hand out stays, unclaimed (ClaimSession), for the next
// presentation of its predecessor to complete.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	// ClaimSession records that a refresh hands the session's token out: it
	// stamps delivered_at with the store's clock, once — a session already
	// claimed keeps its first stamp — and only on a session that is not
	// revoked. claimed is false, and nothing is written, when the session is
	// revoked or gone.
	ClaimSession(ctx context.Context, id string) (claimed bool, err error)
	// RevokeSession signs one session out: it writes revoked_at only when the
	// field is absent — the instant a session was retired is never moved —
	// and clears the rotation record, so the successor that record names can
	// no longer be redelivered or written. It returns the record it cleared
	// (zero when there was none); a session that is gone is
	// ErrSessionNotFound.
	RevokeSession(ctx context.Context, id string, at time.Time) (cleared Rotation, err error)
	// RevokeSessionIfNotRevoked is a compare-and-set on revoked_at:
	// the write only lands when the field is currently absent.
	// Returns revoked=true when this call performed the revocation,
	// false when the session had already been revoked by a concurrent
	// caller.
	RevokeSessionIfNotRevoked(ctx context.Context, id string, at time.Time) (revoked bool, err error)
	// RotateSession is the rotation's compare-and-set: it writes
	// revoked_at, stamped with the store's own clock (Now), AND the rotation
	// record (rot) in one write, only when revoked_at is currently absent.
	// Returns the stamp and rotated=true when this call retired the session;
	// rotated=false when it was already revoked — by a concurrent rotation
	// or by a sign-out — or is gone, in which case nothing is written.
	RotateSession(ctx context.Context, id string, rot Rotation) (rotatedAt time.Time, rotated bool, err error)
	// RecordLineageRevocation records the lineage lineageID of userID as
	// revoked, for good: SessionRevoked answers true for every session of it
	// from then on. Recording a lineage again changes nothing. It is the
	// commit point of a sign-out and of reuse detection.
	RecordLineageRevocation(ctx context.Context, userID, lineageID string, at time.Time) error
	// RevokeLineageSessions is the pass of a lineage revocation: it writes
	// what every revocation writes (revoked_at where it is absent, the lineage
	// stamped where it is absent) on the sessions of the lineage that are
	// still live, and on its root when that was written before lineages, and
	// returns how many it matched. A retired session is left as it is: the
	// lineage's record refuses the successor its rotation record names. Other
	// lineages of the user are untouched. Idempotent.
	RevokeLineageSessions(ctx context.Context, userID, lineageID string, at time.Time) (revoked int64, err error)
	// LiveLineageSession reads the lineage's LIVE session row — the same
	// match RevokeLineageSessions revokes (the user's sessions of the
	// lineage whose revoked_at is absent, plus a root written before
	// lineages whose id is the lineage id) — read, never written. A lineage
	// holds at most one live row: a rotation retires its predecessor in the
	// same write that delivers the successor. A store that ever holds more
	// answers the newest by issued_at. found is false, with a zero session
	// and a nil error, when none is live; a read that failed is the error
	// it is.
	LiveLineageSession(ctx context.Context, userID, lineageID string) (sess Session, found bool, err error)
	// RecordAccountRevocation raises the account's session generation and
	// returns the generation it raised it to: SessionRevoked answers true for
	// every session born under a lower one from then on, and a session born
	// under it is not covered. It is the commit point of "sign out
	// everywhere", of a credential change and of an admin disable.
	RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (generation int64, err error)
	// RevokeAccountSessions is the pass of an account revocation that raised
	// the generation to below: it writes what every revocation writes on the
	// sessions of userID born under a generation lower than below that are
	// still live or were written before lineages, and returns how many it
	// matched. A session born under below or later — the one a credential
	// change opens for its caller — is untouched, and so is a retired one: the
	// account's record refuses the successor its rotation record names.
	// Idempotent.
	RevokeAccountSessions(ctx context.Context, userID string, below int64, at time.Time) (revoked int64, err error)
	// ClaimCredentialChange takes the account's credential-change claim for
	// owner for ttl — the claim a password change, a reset confirm, a forced
	// rotation and an admin reset hold while they revoke and commit, so two
	// of them on one account never interleave. claimed is false, with a nil
	// error, while another owner holds it unexpired. A claim is only as
	// exclusive as its holder's bound: a holder must finish within ttl.
	ClaimCredentialChange(ctx context.Context, userID, owner string, ttl time.Duration) (claimed bool, err error)
	// ReleaseCredentialChange gives the claim up when owner holds it.
	ReleaseCredentialChange(ctx context.Context, userID, owner string) error
	// SessionGeneration reads the account's session generation: zero until
	// its first revoke-all. IssueSession stamps it on the root it writes.
	SessionGeneration(ctx context.Context, userID string) (int64, error)
	// SessionRevoked reports whether a revocation recorded by RevokeLineage
	// or RevokeUserSessions covers s: its lineage revoked, or its account's
	// generation raised above the one s was born under.
	SessionRevoked(ctx context.Context, s Session) (bool, error)
	// DeleteExpired deletes every session whose expires_at is before
	// `before`; it returns the number of sessions deleted. Revocation
	// records are kept.
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
	// Now reads the store's clock — the clock RotateSession stamps a
	// rotation with — so the reuse window is measured on one clock, whichever
	// replica rotated the session and whichever answers its presentation.
	// The stamp and the read come from the same node: the Mongo twin reads
	// from the primary, which every rotation writes to.
	Now(ctx context.Context) (time.Time, error)
}

// Rotation is what a refresh records on the session it retires: the nonce
// and the server key id its successor token was derived under, the
// successor's token hash, and the client the rotation was for.
type Rotation struct {
	Nonce         string
	KeyID         string
	SuccessorHash string
	UserAgent     string
	IP            string
}

// sessionOpTimeout bounds every session-store step a refresh or a switch
// takes — the reads of the presented token and of its successor, the store's
// clock, the rotation, the successor's write, its check and its claim, a
// disabled account's retirement — inside the refresh's own bound
// (RefreshTimeout), so a store that stalls answers a failure within seconds:
// before the rotation, the presented token stays live; after it, the
// successor stays unwritten or unclaimed, and the next presentation of the
// token completes it within the completion horizon.
const sessionOpTimeout = 5 * time.Second

// RefreshTimeout bounds a refresh as a whole — its session-store steps and
// its identity reads alike — so the slowest refresh that succeeds answers
// within it. A refusal that revokes (reuse detection) adds its revocation,
// bounded by revocationTimeout, which runs whether or not the client still
// waits.
const RefreshTimeout = 20 * time.Second

// Sentinel errors used by the SessionStore implementations.
var (
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrSessionRevoked  = errors.New("auth: session revoked")
	ErrSessionExpired  = errors.New("auth: session expired")
	// ErrSessionExists is CreateSession's refusal of a token hash the store
	// already holds. A random token never collides; a derived successor
	// does when another refresh of the same token wrote it first.
	ErrSessionExists = errors.New("auth: session token hash already exists")
)

// refreshTokenBytes is the entropy of a refresh token, random or derived.
const refreshTokenBytes = 48

// HashRefreshToken returns the hex SHA-256 of a plaintext refresh
// token. Stored on the Session and consulted at refresh time.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SessionRoot names a lineage: the id of its root session — the lineage id
// every successor copies — and the account session generation it is born
// under. A login resolves its root before it commits anything (NewSessionRoot,
// NewAccountRoot) and signs it into the access token it answers with, so a
// switch can check the token against the revocations of its session.
type SessionRoot struct {
	ID         string
	Generation int64
}

// NewSessionRoot names the root a login of an existing account will write:
// a fresh id, born under the account's current session generation. The
// generation is read before the root is written: a revoke-all that raises it
// after the read covers the root (SessionRevoked), so a login racing a
// revoke-all is never left outside it. A password sign-in reads it before the
// stamp that commits it to the password it verified (Service.Login), so the
// raise a credential change makes after its commit covers a sign-in it raced.
func NewSessionRoot(ctx context.Context, store SessionStore, userID string) (SessionRoot, error) {
	generation, err := store.SessionGeneration(ctx, userID)
	if err != nil {
		return SessionRoot{}, fmt.Errorf("auth: read the session generation of user %s: %w", userID, err)
	}
	return SessionRoot{ID: uuid.NewString(), Generation: generation}, nil
}

// NewAccountRoot names the root of the first login of an account the same
// call creates: generation zero, read from nothing — no revocation can name an
// account id minted a moment ago — so the login reads nothing of that
// account. A revoke-all landing once the account exists raises the generation
// above zero and covers the root.
func NewAccountRoot() SessionRoot {
	return SessionRoot{ID: uuid.NewString()}
}

// RevokedAccountRoot names the root of the session a credential change opens
// once its new password is committed: born under the generation the change's
// last revocation raised the account to, so the change covers every session
// but this one.
func RevokedAccountRoot(generation int64) SessionRoot {
	return SessionRoot{ID: uuid.NewString(), Generation: generation}
}

// IssueSession generates a fresh refresh token, persists the hashed session
// — root, the root of a new lineage — and returns the plaintext token to the
// caller. The caller is responsible for setting the cookie / sending it to
// the client. The root is delivered by the login that writes it.
func IssueSession(ctx context.Context, store SessionStore, root SessionRoot, userID, userAgent, ip string, ttl time.Duration) (token string, sess Session, err error) {
	rawTok, _, err := GenerateRandomToken(refreshTokenBytes)
	if err != nil {
		return "", Session{}, fmt.Errorf("auth: gen refresh: %w", err)
	}
	if root.ID == "" {
		return "", Session{}, errors.New("auth: issue a session: no root id")
	}
	now := time.Now().UTC()
	sess = Session{
		ID:          root.ID,
		UserID:      userID,
		TokenHash:   HashRefreshToken(rawTok),
		UserAgent:   userAgent,
		IP:          ip,
		IssuedAt:    now,
		ExpiresAt:   now.Add(ttl),
		DeliveredAt: &now,
		LineageID:   root.ID,
		Generation:  root.Generation,
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	return rawTok, sess, nil
}

// MemorySessionStore is the in-memory SessionStore for tests.
type MemorySessionStore struct {
	mu       sync.Mutex
	byID     map[string]Session
	byHash   map[string]string // token hash → session id
	byUserID map[string]map[string]bool
	// generations is each account's session generation; revokedLineages
	// holds every revoked lineage.
	generations     map[string]int64
	revokedLineages map[string]bool
	clock           func() time.Time
	// claims holds the credential-change claims (ClaimCredentialChange).
	claims *lease.MemoryStore
}

// NewMemorySessionStore returns an empty in-memory store on the process
// clock.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		byID:            map[string]Session{},
		byHash:          map[string]string{},
		byUserID:        map[string]map[string]bool{},
		generations:     map[string]int64{},
		revokedLineages: map[string]bool{},
		clock:           time.Now,
		claims:          lease.NewMemoryStore(),
	}
}

// SetClock replaces the store's clock — the one Now reads and RotateSession
// stamps with — so a test can move it by hand.
func (m *MemorySessionStore) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clock = now
}

// storeNow is the store's clock at millisecond precision, the precision
// Mongo stores a date at. Callers hold m.mu.
func (m *MemorySessionStore) storeNow() time.Time {
	return m.clock().UTC().Truncate(time.Millisecond)
}

func (m *MemorySessionStore) Now(_ context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.storeNow(), nil
}

func (m *MemorySessionStore) CreateSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byHash[s.TokenHash]; ok {
		return fmt.Errorf("%w: refresh token hash collision", ErrSessionExists)
	}
	m.byID[s.ID] = s
	m.byHash[s.TokenHash] = s.ID
	if m.byUserID[s.UserID] == nil {
		m.byUserID[s.UserID] = make(map[string]bool)
	}
	m.byUserID[s.UserID][s.ID] = true
	return nil
}

func (m *MemorySessionStore) GetSessionByTokenHash(_ context.Context, tokenHash string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byHash[tokenHash]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return m.byID[id], nil
}

func (m *MemorySessionStore) ClaimSession(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok || s.RevokedAt != nil {
		return false, nil
	}
	if s.DeliveredAt == nil {
		at := m.storeNow()
		s.DeliveredAt = &at
		m.byID[id] = s
	}
	return true, nil
}

// removeLocked drops s from every index. Callers hold m.mu.
func (m *MemorySessionStore) removeLocked(s Session) {
	delete(m.byID, s.ID)
	if m.byHash[s.TokenHash] == s.ID {
		delete(m.byHash, s.TokenHash)
	}
	if set := m.byUserID[s.UserID]; set != nil {
		delete(set, s.ID)
	}
}

// revokeLocked is what every revocation writes on one session: revoked_at
// where it is absent, the rotation record cleared, the lineage stamped where
// it is absent. Callers hold m.mu.
func (m *MemorySessionStore) revokeLocked(s Session, at time.Time) Session {
	if s.RevokedAt == nil {
		s.RevokedAt = &at
	}
	if s.LineageID == "" {
		s.LineageID = s.ID
	}
	s.clearRotation()
	m.byID[s.ID] = s
	return s
}

func (m *MemorySessionStore) RevokeSession(_ context.Context, id string, at time.Time) (Rotation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok {
		return Rotation{}, ErrSessionNotFound
	}
	cleared := s.rotation()
	m.revokeLocked(s, at)
	return cleared, nil
}

func (m *MemorySessionStore) RevokeSessionIfNotRevoked(_ context.Context, id string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok {
		return false, ErrSessionNotFound
	}
	if s.RevokedAt != nil {
		return false, nil
	}
	s.RevokedAt = &at
	m.byID[id] = s
	return true, nil
}

func (m *MemorySessionStore) RotateSession(_ context.Context, id string, rot Rotation) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok || s.RevokedAt != nil {
		return time.Time{}, false, nil
	}
	at := m.storeNow()
	s.RevokedAt = &at
	if s.LineageID == "" {
		s.LineageID = s.ID
	}
	s.RotationNonce = rot.Nonce
	s.RotationKeyID = rot.KeyID
	s.SuccessorHash = rot.SuccessorHash
	s.RotationUserAgent = rot.UserAgent
	s.RotationIP = rot.IP
	m.byID[id] = s
	return at, true, nil
}

func (m *MemorySessionStore) RecordLineageRevocation(_ context.Context, _ string, lineageID string, _ time.Time) error {
	if lineageID == "" {
		return errors.New("auth: revoke a lineage: no lineage id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedLineages[lineageID] = true
	return nil
}

func (m *MemorySessionStore) RevokeLineageSessions(_ context.Context, userID, lineageID string, at time.Time) (int64, error) {
	if lineageID == "" {
		return 0, errors.New("auth: revoke a lineage: no lineage id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id := range m.byUserID[userID] {
		s := m.byID[id]
		live := s.RevokedAt == nil && s.lineage() == lineageID
		preLineageRoot := s.LineageID == "" && s.ID == lineageID
		if live || preLineageRoot {
			m.revokeLocked(s, at)
			n++
		}
	}
	return n, nil
}

// LiveLineageSession reads the lineage's live row through the same predicate
// RevokeLineageSessions writes through, over the user's own rows only. The
// newest-by-issued_at answer keeps the read deterministic should a store
// ever hold more live rows than the one-row invariant allows.
func (m *MemorySessionStore) LiveLineageSession(_ context.Context, userID, lineageID string) (Session, bool, error) {
	if lineageID == "" {
		return Session{}, false, errors.New("auth: read the live session of a lineage: no lineage id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best Session
	found := false
	for id := range m.byUserID[userID] {
		s := m.byID[id]
		live := s.RevokedAt == nil && s.lineage() == lineageID
		preLineageRoot := s.LineageID == "" && s.ID == lineageID
		if !live && !preLineageRoot {
			continue
		}
		if !found || s.IssuedAt.After(best.IssuedAt) {
			best, found = s, true
		}
	}
	if !found {
		return Session{}, false, nil
	}
	return best, true, nil
}

func (m *MemorySessionStore) RecordAccountRevocation(_ context.Context, userID string, _ time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generations[userID]++
	return m.generations[userID], nil
}

func (m *MemorySessionStore) RevokeAccountSessions(_ context.Context, userID string, below int64, at time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id := range m.byUserID[userID] {
		s := m.byID[id]
		if s.Generation >= below || (s.RevokedAt != nil && s.LineageID != "") {
			continue
		}
		m.revokeLocked(s, at)
		n++
	}
	return n, nil
}

func (m *MemorySessionStore) ClaimCredentialChange(ctx context.Context, userID, owner string, ttl time.Duration) (bool, error) {
	return m.claims.Acquire(ctx, credentialChangeLease(userID), owner, m.clockNow(), ttl)
}

func (m *MemorySessionStore) ReleaseCredentialChange(ctx context.Context, userID, owner string) error {
	return m.claims.Release(ctx, credentialChangeLease(userID), owner)
}

// clockNow reads the store's clock outside the lock.
func (m *MemorySessionStore) clockNow() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clock()
}

// credentialChangeLease names the lease that holds an account's
// credential-change claim (SessionStore.ClaimCredentialChange).
func credentialChangeLease(userID string) string {
	return "auth/credential-change/" + userID
}

func (m *MemorySessionStore) SessionGeneration(_ context.Context, userID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generations[userID], nil
}

func (m *MemorySessionStore) SessionRevoked(_ context.Context, s Session) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revokedLineages[s.lineage()] {
		return true, nil
	}
	return m.generations[s.UserID] > s.Generation, nil
}

func (m *MemorySessionStore) DeleteExpired(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, s := range m.byID {
		if s.ExpiresAt.Before(before) {
			m.removeLocked(s)
			n++
		}
	}
	return n, nil
}

// MongoSessionStore implements SessionStore on Mongo.
//
// Every read it makes goes to the primary, whatever read preference the
// client's URI names: the store reads back what it just wrote — a rotation's
// compare-and-set, a successor's check against the revocations that may
// cover it (SessionRevoked), the clock a rotation was stamped with (Now) —
// and a secondary may not hold it yet.
type MongoSessionStore struct {
	db          *mongo.Database
	coll        *mongo.Collection
	revocations *mongo.Collection
	// claims holds the credential-change claims (ClaimCredentialChange) in
	// the lease collection of the same database.
	claims *lease.MongoStore
}

// SessionsCollectionName is pinned for monitoring/migration tooling.
const SessionsCollectionName = "sessions"

// SessionRevocationsCollectionName holds the revocations a SessionStore
// records before it revokes: one row per revoked lineage and one row per
// account that was signed out everywhere, holding its session generation.
// No row expires: a lineage's row covers the sessions of a pass that did not
// complete for as long as they live, and how long that is was set by the
// refresh TTL configured when each was written, which no later revocation
// can know.
const SessionRevocationsCollectionName = "session_revocations"

// sessionRevocation is a row of SessionRevocationsCollectionName: a revoked
// lineage (ID lineageRevocationID) or an account's session generation (ID
// accountRevocationID).
type sessionRevocation struct {
	ID         string    `bson:"_id"`
	UserID     string    `bson:"user_id"`
	Generation int64     `bson:"generation,omitempty"`
	RevokedAt  time.Time `bson:"revoked_at"`
}

func accountRevocationID(userID string) string { return "account:" + userID }

func lineageRevocationID(lineageID string) string { return "lineage:" + lineageID }

// NewMongoSessionStore creates the store on db, every read pinned to the
// primary; EnsureSchema sets up its indexes.
func NewMongoSessionStore(db *mongo.Database) *MongoSessionStore {
	primary := db.Client().Database(db.Name(), options.Database().SetReadPreference(readpref.Primary()))
	return &MongoSessionStore{
		db:          primary,
		coll:        primary.Collection(SessionsCollectionName),
		revocations: primary.Collection(SessionRevocationsCollectionName),
		claims:      lease.NewMongoStore(primary),
	}
}

// EnsureSchema creates the indexes used by the store. Safe to call
// repeatedly. The TTL index drives Mongo's own expirator, so stale sessions
// disappear without an explicit DeleteExpired sweep; revocation rows are
// read by their _id and never expire.
func (s *MongoSessionStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "issued_at", Value: -1}}, Options: options.Index().SetName("user_issued")},
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "lineage", Value: 1}}, Options: options.Index().SetName("user_lineage")},
		// The passes of a revocation (RevokeAccountSessions,
		// RevokeLineageSessions) read the live rows through these two.
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "revoked_at", Value: 1}}, Options: options.Index().SetName("user_live")},
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "lineage", Value: 1}, {Key: "revoked_at", Value: 1}}, Options: options.Index().SetName("user_lineage_live")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("sessions_ttl").SetExpireAfterSeconds(0)},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("auth: ensure sessions indexes: %w", err)
	}
	return nil
}

func (s *MongoSessionStore) CreateSession(ctx context.Context, sess Session) error {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	_, err := s.coll.InsertOne(ctx, sess)
	if mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("auth: insert session: %w: %w", ErrSessionExists, err)
	}
	if err != nil {
		return fmt.Errorf("auth: insert session: %w", err)
	}
	return nil
}

func (s *MongoSessionStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	return mongoutil.FindOne[Session](ctx, s.coll, bson.M{"token_hash": tokenHash}, ErrSessionNotFound, "auth: get session")
}

func (s *MongoSessionStore) ClaimSession(ctx context.Context, id string) (bool, error) {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}},
		mongo.Pipeline{{{Key: "$set", Value: bson.M{
			"delivered_at": bson.M{"$ifNull": bson.A{"$delivered_at", "$$NOW"}},
		}}}},
	)
	if err != nil {
		return false, fmt.Errorf("auth: claim session %s: %w", id, err)
	}
	return res.MatchedCount > 0, nil
}

// revocationPipeline is what every revocation writes on the sessions it
// matches: revoked_at where it is absent — the instant a session was retired
// is never moved — the lineage stamped where it is absent, and the rotation
// record cleared.
func revocationPipeline(at time.Time) mongo.Pipeline {
	return mongo.Pipeline{
		{{Key: "$set", Value: bson.M{
			"revoked_at": bson.M{"$ifNull": bson.A{"$revoked_at", at}},
			"lineage":    bson.M{"$ifNull": bson.A{"$lineage", "$_id"}},
		}}},
		{{Key: "$unset", Value: rotationRecordFields}},
	}
}

func (s *MongoSessionStore) RevokeSession(ctx context.Context, id string, at time.Time) (Rotation, error) {
	// One write that returns the row as it was before it: the record cleared
	// is the record that stood when the sign-out landed, not one read
	// earlier, which a rotation landing in between would have missed.
	var before Session
	err := s.coll.FindOneAndUpdate(ctx,
		bson.M{"_id": id},
		revocationPipeline(at),
		options.FindOneAndUpdate().SetReturnDocument(options.Before),
	).Decode(&before)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Rotation{}, ErrSessionNotFound
	}
	if err != nil {
		return Rotation{}, fmt.Errorf("auth: revoke session: %w", err)
	}
	return before.rotation(), nil
}

func (s *MongoSessionStore) RevokeSessionIfNotRevoked(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"revoked_at": at}},
	)
	if err != nil {
		return false, fmt.Errorf("auth: revoke session if-not-revoked: %w", err)
	}
	return res.MatchedCount > 0, nil
}

func (s *MongoSessionStore) RotateSession(ctx context.Context, id string, rot Rotation) (time.Time, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	// A pipeline is what makes $$NOW legal; every value it writes from the
	// request is a $literal, so none is ever read as a field path.
	var after Session
	err := s.coll.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}},
		mongo.Pipeline{{{Key: "$set", Value: bson.M{
			"revoked_at":     "$$NOW",
			"lineage":        bson.M{"$ifNull": bson.A{"$lineage", "$_id"}},
			"rotation_nonce": bson.M{"$literal": rot.Nonce},
			"rotation_kid":   bson.M{"$literal": rot.KeyID},
			"successor_hash": bson.M{"$literal": rot.SuccessorHash},
			"rotation_ua":    bson.M{"$literal": rot.UserAgent},
			"rotation_ip":    bson.M{"$literal": rot.IP},
		}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"revoked_at": 1}),
	).Decode(&after)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("auth: rotate session: %w", err)
	}
	if after.RevokedAt == nil {
		return time.Time{}, false, fmt.Errorf("auth: rotate session %s: the rotation wrote no revoked_at", id)
	}
	return after.RevokedAt.UTC(), true, nil
}

func (s *MongoSessionStore) RecordLineageRevocation(ctx context.Context, userID, lineageID string, at time.Time) error {
	if lineageID == "" {
		return errors.New("auth: revoke a lineage: no lineage id")
	}
	if _, err := s.revocations.UpdateOne(ctx,
		bson.M{"_id": lineageRevocationID(lineageID)},
		bson.M{"$setOnInsert": bson.M{"user_id": userID, "revoked_at": at}},
		options.UpdateOne().SetUpsert(true),
	); err != nil {
		return fmt.Errorf("auth: revoke lineage %s: record the revocation: %w", lineageID, err)
	}
	return nil
}

// RevokeLineageSessions matches the lineage's live rows through the
// user_lineage_live index and its pre-lineage root by _id, so its cost follows
// the rows that can still answer something, never the lineage's history: a
// retired row has a revoked_at, which a null match excludes, and revoked_at
// is never stored as an explicit null.
func (s *MongoSessionStore) RevokeLineageSessions(ctx context.Context, userID, lineageID string, at time.Time) (int64, error) {
	if lineageID == "" {
		return 0, errors.New("auth: revoke a lineage: no lineage id")
	}
	res, err := s.coll.UpdateMany(ctx,
		bson.M{"$or": bson.A{
			bson.M{"user_id": userID, "lineage": lineageID, "revoked_at": nil},
			bson.M{"user_id": userID, "_id": lineageID, "lineage": nil},
		}},
		revocationPipeline(at),
	)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke the sessions of lineage %s: %w", lineageID, err)
	}
	return res.MatchedCount, nil
}

// LiveLineageSession reads through the same match RevokeLineageSessions
// writes through — the user_lineage_live index and the pre-lineage root by
// _id — decoding the session's own fields; newest by issued_at keeps the
// answer deterministic should a store ever hold more live rows than the
// one-row invariant allows.
func (s *MongoSessionStore) LiveLineageSession(ctx context.Context, userID, lineageID string) (Session, bool, error) {
	if lineageID == "" {
		return Session{}, false, errors.New("auth: read the live session of a lineage: no lineage id")
	}
	var sess Session
	err := s.coll.FindOne(ctx,
		bson.M{"$or": bson.A{
			bson.M{"user_id": userID, "lineage": lineageID, "revoked_at": nil},
			bson.M{"user_id": userID, "_id": lineageID, "lineage": nil},
		}},
		options.FindOne().SetSort(bson.M{"issued_at": -1}),
	).Decode(&sess)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("auth: read the live session of lineage %s: %w", lineageID, err)
	}
	return sess, true, nil
}

func (s *MongoSessionStore) RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (int64, error) {
	var raised sessionRevocation
	if err := s.revocations.FindOneAndUpdate(ctx,
		bson.M{"_id": accountRevocationID(userID)},
		bson.M{
			"$inc": bson.M{"generation": int64(1)},
			"$set": bson.M{"user_id": userID, "revoked_at": at},
		},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After).SetProjection(bson.M{"generation": 1}),
	).Decode(&raised); err != nil {
		return 0, fmt.Errorf("auth: revoke user sessions: raise the session generation: %w", err)
	}
	return raised.Generation, nil
}

// RevokeAccountSessions matches the account's live rows through the
// user_live index and its pre-lineage rows through user_lineage, then keeps
// those born under a lower generation (a row of generation zero stores none):
// its cost follows the rows that can still answer something, never the
// account's refresh history.
func (s *MongoSessionStore) RevokeAccountSessions(ctx context.Context, userID string, below int64, at time.Time) (int64, error) {
	bornBelow := bson.A{
		bson.M{"generation": bson.M{"$lt": below}},
		bson.M{"generation": bson.M{"$exists": false}},
	}
	res, err := s.coll.UpdateMany(ctx,
		bson.M{"$or": bson.A{
			bson.M{"user_id": userID, "revoked_at": nil, "$or": bornBelow},
			bson.M{"user_id": userID, "lineage": nil, "$or": bornBelow},
		}},
		revocationPipeline(at),
	)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke the sessions of user %s: %w", userID, err)
	}
	return res.MatchedCount, nil
}

func (s *MongoSessionStore) ClaimCredentialChange(ctx context.Context, userID, owner string, ttl time.Duration) (bool, error) {
	return s.claims.Acquire(ctx, credentialChangeLease(userID), owner, time.Now(), ttl)
}

func (s *MongoSessionStore) ReleaseCredentialChange(ctx context.Context, userID, owner string) error {
	return s.claims.Release(ctx, credentialChangeLease(userID), owner)
}

func (s *MongoSessionStore) SessionGeneration(ctx context.Context, userID string) (int64, error) {
	var row sessionRevocation
	err := s.revocations.FindOne(ctx, bson.M{"_id": accountRevocationID(userID)}).Decode(&row)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("auth: read the session generation: %w", err)
	}
	return row.Generation, nil
}

func (s *MongoSessionStore) SessionRevoked(ctx context.Context, sess Session) (bool, error) {
	account, lineage := accountRevocationID(sess.UserID), lineageRevocationID(sess.lineage())
	cur, err := s.revocations.Find(ctx, bson.M{"_id": bson.M{"$in": bson.A{account, lineage}}})
	if err != nil {
		return false, fmt.Errorf("auth: read the revocations of session %s: %w", sess.ID, err)
	}
	var rows []sessionRevocation
	if err := cur.All(ctx, &rows); err != nil {
		return false, fmt.Errorf("auth: decode the revocations of session %s: %w", sess.ID, err)
	}
	for _, r := range rows {
		if r.ID == lineage || (r.ID == account && r.Generation > sess.Generation) {
			return true, nil
		}
	}
	return false, nil
}

func (s *MongoSessionStore) Now(ctx context.Context) (time.Time, error) {
	cur, err := s.db.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$documents", Value: bson.A{bson.M{}}}},
		{{Key: "$project", Value: bson.M{"now": "$$NOW"}}},
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: read the session store's clock: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var rows []struct {
		Now time.Time `bson:"now"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return time.Time{}, fmt.Errorf("auth: decode the session store's clock: %w", err)
	}
	if len(rows) != 1 || rows[0].Now.IsZero() {
		return time.Time{}, fmt.Errorf("auth: the session store's clock answered %d row(s)", len(rows))
	}
	return rows[0].Now.UTC(), nil
}

func (s *MongoSessionStore) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.coll.DeleteMany(ctx, bson.M{"expires_at": bson.M{"$lt": before}})
	if err != nil {
		return 0, fmt.Errorf("auth: delete expired sessions: %w", err)
	}
	return res.DeletedCount, nil
}
