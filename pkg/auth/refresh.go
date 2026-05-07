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

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Session is a stored refresh token. The plaintext token is never
// persisted — only its SHA-256 hash. On rotation, the previous
// session is marked Revoked and a new one is created.
type Session struct {
	ID            string     `bson:"_id" json:"id"`
	UserID        string     `bson:"user_id" json:"user_id"`
	TokenHash     string     `bson:"token_hash" json:"-"`
	UserAgent     string     `bson:"user_agent,omitempty" json:"user_agent,omitempty"`
	IP            string     `bson:"ip,omitempty" json:"ip,omitempty"`
	IssuedAt      time.Time  `bson:"issued_at" json:"issued_at"`
	ExpiresAt     time.Time  `bson:"expires_at" json:"expires_at"`
	RevokedAt     *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	RotatedFromID string     `bson:"rotated_from,omitempty" json:"-"`
}

// SessionStore is the persistence interface for refresh tokens.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	RevokeSession(ctx context.Context, id string, at time.Time) error
	RevokeUserSessions(ctx context.Context, userID string, at time.Time) error
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// Sentinel errors used by the SessionStore implementations.
var (
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrSessionRevoked  = errors.New("auth: session revoked")
	ErrSessionExpired  = errors.New("auth: session expired")
)

// HashRefreshToken returns the hex SHA-256 of a plaintext refresh
// token. Stored on the Session and consulted at refresh time.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueSession generates a fresh refresh token, persists the hashed
// session, and returns the plaintext token to the caller. The caller
// is responsible for setting the cookie / sending it to the client.
func IssueSession(ctx context.Context, store SessionStore, userID, userAgent, ip string, ttl time.Duration) (token string, sess Session, err error) {
	rawTok, _, err := GenerateRandomToken(48)
	if err != nil {
		return "", Session{}, fmt.Errorf("auth: gen refresh: %w", err)
	}
	now := time.Now().UTC()
	sess = Session{
		ID:        uuid.NewString(),
		UserID:    userID,
		TokenHash: HashRefreshToken(rawTok),
		UserAgent: userAgent,
		IP:        ip,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	return rawTok, sess, nil
}

// RotateSession atomically validates an incoming refresh token,
// revokes the previous session, and issues a new one. Returns the
// new plaintext refresh token + the new Session.
//
// Validation failures map to ErrSessionNotFound (token unknown),
// ErrSessionRevoked (already used / explicitly revoked), or
// ErrSessionExpired (past TTL).
func RotateSession(ctx context.Context, store SessionStore, presentedToken, userAgent, ip string, ttl time.Duration) (newToken string, newSess Session, prev Session, err error) {
	hash := HashRefreshToken(presentedToken)
	prev, err = store.GetSessionByTokenHash(ctx, hash)
	if err != nil {
		return "", Session{}, Session{}, err
	}
	now := time.Now().UTC()
	if prev.RevokedAt != nil {
		return "", Session{}, prev, ErrSessionRevoked
	}
	if !prev.ExpiresAt.IsZero() && now.After(prev.ExpiresAt) {
		return "", Session{}, prev, ErrSessionExpired
	}
	if err := store.RevokeSession(ctx, prev.ID, now); err != nil {
		return "", Session{}, prev, fmt.Errorf("auth: revoke previous: %w", err)
	}
	rawTok, _, err := GenerateRandomToken(48)
	if err != nil {
		return "", Session{}, prev, fmt.Errorf("auth: gen refresh: %w", err)
	}
	newSess = Session{
		ID:            uuid.NewString(),
		UserID:        prev.UserID,
		TokenHash:     HashRefreshToken(rawTok),
		UserAgent:     userAgent,
		IP:            ip,
		IssuedAt:      now,
		ExpiresAt:     now.Add(ttl),
		RotatedFromID: prev.ID,
	}
	if err := store.CreateSession(ctx, newSess); err != nil {
		return "", Session{}, prev, fmt.Errorf("auth: create session: %w", err)
	}
	return rawTok, newSess, prev, nil
}

// MemorySessionStore is the in-memory SessionStore for tests.
type MemorySessionStore struct {
	mu       sync.Mutex
	byID     map[string]Session
	byHash   map[string]string // token hash → session id
	byUserID map[string]map[string]bool
}

// NewMemorySessionStore returns an empty in-memory store.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		byID:     map[string]Session{},
		byHash:   map[string]string{},
		byUserID: map[string]map[string]bool{},
	}
}

func (m *MemorySessionStore) CreateSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byHash[s.TokenHash]; ok {
		return errors.New("auth: refresh token hash collision")
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

func (m *MemorySessionStore) RevokeSession(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok {
		return ErrSessionNotFound
	}
	t := at
	s.RevokedAt = &t
	m.byID[id] = s
	return nil
}

func (m *MemorySessionStore) RevokeUserSessions(_ context.Context, userID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.byUserID[userID] {
		s := m.byID[id]
		if s.RevokedAt == nil {
			t := at
			s.RevokedAt = &t
			m.byID[id] = s
		}
	}
	return nil
}

func (m *MemorySessionStore) DeleteExpired(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, s := range m.byID {
		if s.ExpiresAt.Before(before) {
			delete(m.byID, id)
			delete(m.byHash, s.TokenHash)
			if set := m.byUserID[s.UserID]; set != nil {
				delete(set, id)
			}
			n++
		}
	}
	return n, nil
}

// MongoSessionStore implements SessionStore on Mongo.
type MongoSessionStore struct {
	coll *mongo.Collection
}

// SessionsCollectionName is pinned for monitoring/migration tooling.
const SessionsCollectionName = "sessions"

// NewMongoSessionStore creates the store; EnsureSessionSchema sets up
// indexes (TTL on expires_at, unique on token_hash).
func NewMongoSessionStore(db *mongo.Database) *MongoSessionStore {
	return &MongoSessionStore{coll: db.Collection(SessionsCollectionName)}
}

// EnsureSessionSchema creates the indexes used by the store. Safe
// to call repeatedly. The TTL index drives Mongo's own expirator so
// stale sessions disappear without an explicit DeleteExpired sweep.
func (s *MongoSessionStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "issued_at", Value: -1}}, Options: options.Index().SetName("user_issued")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("sessions_ttl").SetExpireAfterSeconds(0)},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("auth: ensure sessions indexes: %w", err)
	}
	return nil
}

func (s *MongoSessionStore) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.coll.InsertOne(ctx, sess)
	if err != nil {
		return fmt.Errorf("auth: insert session: %w", err)
	}
	return nil
}

func (s *MongoSessionStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	err := s.coll.FindOne(ctx, bson.M{"token_hash": tokenHash}).Decode(&sess)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: get session: %w", err)
	}
	return sess, nil
}

func (s *MongoSessionStore) RevokeSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"revoked_at": at}})
	if err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}

func (s *MongoSessionStore) RevokeUserSessions(ctx context.Context, userID string, at time.Time) error {
	_, err := s.coll.UpdateMany(ctx, bson.M{"user_id": userID, "revoked_at": bson.M{"$exists": false}}, bson.M{"$set": bson.M{"revoked_at": at}})
	if err != nil {
		return fmt.Errorf("auth: revoke user sessions: %w", err)
	}
	return nil
}

func (s *MongoSessionStore) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.coll.DeleteMany(ctx, bson.M{"expires_at": bson.M{"$lt": before}})
	if err != nil {
		return 0, fmt.Errorf("auth: delete expired sessions: %w", err)
	}
	return res.DeletedCount, nil
}
