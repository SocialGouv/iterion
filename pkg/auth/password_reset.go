package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// ResetTokenPrefix marks an iterion password-reset token (one-shot,
// short-lived; emailed to the account address).
const ResetTokenPrefix = "iar_"

// ResetTokenTTL bounds how long a reset link stays valid.
const ResetTokenTTL = 60 * time.Minute

// PasswordReset is one pending reset at rest. The plaintext token
// travels only in the email; only the hash is persisted.
type PasswordReset struct {
	ID         string     `bson:"_id"`
	UserID     string     `bson:"user_id"`
	TokenHash  string     `bson:"token_hash"`
	CreatedAt  time.Time  `bson:"created_at"`
	ExpiresAt  time.Time  `bson:"expires_at"`
	ConsumedAt *time.Time `bson:"consumed_at,omitempty"`
}

// ErrResetNotFound is the store's generic miss (callers collapse it
// into ErrInvalidCredentials — a reset token is a credential).
var ErrResetNotFound = errors.New("auth: reset token not found")

// PasswordResetStore persists pending resets. Mongo in production;
// memory for tests/local. Keep semantics in lock-step.
type PasswordResetStore interface {
	Create(ctx context.Context, p PasswordReset) error
	GetByTokenHash(ctx context.Context, hash string) (PasswordReset, error)
	// Consume atomically marks the reset used; ok=false when it was
	// already consumed (replay).
	Consume(ctx context.Context, id string, at time.Time) (bool, error)
}

// ---- Mongo store ----

const resetsCollectionName = "password_resets"

type MongoPasswordResetStore struct{ coll *mongo.Collection }

func NewMongoPasswordResetStore(db *mongo.Database) *MongoPasswordResetStore {
	return &MongoPasswordResetStore{coll: db.Collection(resetsCollectionName)}
}

func (s *MongoPasswordResetStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("resets_ttl").SetExpireAfterSeconds(0)},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("auth: ensure password_resets indexes: %w", err)
	}
	return nil
}

func (s *MongoPasswordResetStore) Create(ctx context.Context, p PasswordReset) error {
	if _, err := s.coll.InsertOne(ctx, p); err != nil {
		return fmt.Errorf("auth: insert reset: %w", err)
	}
	return nil
}

func (s *MongoPasswordResetStore) GetByTokenHash(ctx context.Context, hash string) (PasswordReset, error) {
	return mongoutil.FindOne[PasswordReset](ctx, s.coll, bson.M{"token_hash": hash}, ErrResetNotFound, "auth: get reset")
}

func (s *MongoPasswordResetStore) Consume(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": id, "consumed_at": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"consumed_at": at}})
	if err != nil {
		return false, fmt.Errorf("auth: consume reset: %w", err)
	}
	return res.ModifiedCount == 1, nil
}

// ---- Memory store ----

type MemoryPasswordResetStore struct {
	mu     sync.Mutex
	byID   map[string]PasswordReset
	byHash map[string]string
}

func NewMemoryPasswordResetStore() *MemoryPasswordResetStore {
	return &MemoryPasswordResetStore{byID: map[string]PasswordReset{}, byHash: map[string]string{}}
}

func (s *MemoryPasswordResetStore) Create(_ context.Context, p PasswordReset) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[p.ID] = p
	s.byHash[p.TokenHash] = p.ID
	return nil
}

func (s *MemoryPasswordResetStore) GetByTokenHash(_ context.Context, hash string) (PasswordReset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byHash[hash]
	if !ok {
		return PasswordReset{}, ErrResetNotFound
	}
	return s.byID[id], nil
}

func (s *MemoryPasswordResetStore) Consume(_ context.Context, id string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok || p.ConsumedAt != nil {
		return false, nil
	}
	at2 := at
	p.ConsumedAt = &at2
	s.byID[id] = p
	return true, nil
}

// ---- Service methods ----

// RequestPasswordReset mints a one-shot reset token and emails the
// link. ALWAYS returns nil — account enumeration via this endpoint
// must be impossible. Unknown emails are silent even server-side
// (logging them would let an attacker flood the logs with arbitrary
// PII); disabled accounts and store failures are logged server-side
// only. No-op (logged) when the reset store or mailer isn't wired.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	if s.resets == nil || s.mailer == nil {
		s.logf("auth: password reset requested but store/mailer not wired")
		return nil
	}
	norm := identity.NormalizeEmail(email)
	u, err := s.store.GetUserByEmail(ctx, norm)
	if err != nil {
		// Anti-enumeration: an unknown email must be indistinguishable from
		// success at the API — always nil. Server-side logs stay honest:
		// a store outage is an error, only a genuine miss is silent.
		if !errors.Is(err, identity.ErrNotFound) && !errors.Is(err, context.Canceled) {
			s.logf("auth: reset lookup for %s failed: %v", norm, err)
		}
		return nil
	}
	if u.Status == identity.UserStatusDisabled {
		s.logf("auth: reset requested for disabled account %s", u.ID)
		return nil
	}
	tok, _, err := GenerateRandomToken(32)
	if err != nil {
		s.logf("auth: reset token mint: %v", err)
		return nil
	}
	plaintext := ResetTokenPrefix + tok
	now := s.now().UTC()
	rec := PasswordReset{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		TokenHash: HashRefreshToken(plaintext),
		CreatedAt: now,
		ExpiresAt: now.Add(ResetTokenTTL),
	}
	if err := s.resets.Create(ctx, rec); err != nil {
		s.logf("auth: persist reset: %v", err)
		return nil
	}
	msg := mail.RenderPasswordReset(u.Email, mail.ResetData{
		ResetURL:       s.publicURL + "/auth/reset?token=" + plaintext,
		ExpiresMinutes: int(ResetTokenTTL.Minutes()),
	})
	if err := s.mailer.Send(ctx, msg); err != nil {
		s.logf("auth: send reset email to %s: %v", u.Email, err)
	}
	return nil
}

// ConfirmPasswordReset redeems a one-shot token: sets the new
// password, revokes every live session, and issues a fresh login.
func (s *Service) ConfirmPasswordReset(ctx context.Context, token, newPassword, userAgent, ip string) (LoginResult, error) {
	if s.resets == nil {
		return LoginResult{}, ErrInvalidCredentials
	}
	if len(newPassword) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	rec, err := s.resets.GetByTokenHash(ctx, HashRefreshToken(token))
	if err != nil {
		return LoginResult{}, ErrInvalidCredentials
	}
	now := s.now().UTC()
	if now.After(rec.ExpiresAt) {
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := s.resets.Consume(ctx, rec.ID, now)
	if err != nil || !ok {
		return LoginResult{}, ErrInvalidCredentials
	}
	u, err := s.store.GetUser(ctx, rec.UserID)
	if err != nil || u.Status == identity.UserStatusDisabled {
		return LoginResult{}, ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return LoginResult{}, err
	}
	u.PasswordHash = hash
	u.Status = identity.UserStatusActive
	u.FailedLogins = 0
	u.LockedUntil = nil
	u.LastLoginAt = &now
	u.UpdatedAt = now
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	// A reset means the old credential may be compromised — kill every
	// live session before issuing the fresh one.
	if err := s.RevokeUserSessions(ctx, u.ID); err != nil {
		s.logf("auth: revoke sessions on reset for %s: %v", u.ID, err)
	}
	return s.issueLogin(ctx, u, userAgent, ip)
}

// ChangePassword is the authenticated self-service rotation: verify
// the current password, set the new one, revoke every other session,
// and re-issue a login so the caller's session continues seamlessly.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword, userAgent, ip string) (LoginResult, error) {
	if len(newPassword) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil || u.Status != identity.UserStatusActive {
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(currentPassword, u.PasswordHash)
	if err != nil || !ok {
		return LoginResult{}, ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	u.PasswordHash = hash
	u.UpdatedAt = now
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	if err := s.RevokeUserSessions(ctx, u.ID); err != nil {
		s.logf("auth: revoke sessions on password change for %s: %v", u.ID, err)
	}
	return s.issueLogin(ctx, u, userAgent, ip)
}

// logf logs through the service logger when one is wired.
func (s *Service) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}
