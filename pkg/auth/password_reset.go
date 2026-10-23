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
	// Release gives a consumed reset back — only the consumption stamped at
	// at, so a later one is never undone: a confirm that claimed the link
	// and then could not complete leaves it usable.
	Release(ctx context.Context, id string, at time.Time) error
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

func (s *MongoPasswordResetStore) Release(ctx context.Context, id string, at time.Time) error {
	if _, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": id, "consumed_at": at},
		bson.M{"$unset": bson.M{"consumed_at": ""}}); err != nil {
		return fmt.Errorf("auth: release reset: %w", err)
	}
	return nil
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

func (s *MemoryPasswordResetStore) Release(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok || p.ConsumedAt == nil || !p.ConsumedAt.Equal(at) {
		return nil
	}
	p.ConsumedAt = nil
	s.byID[id] = p
	return nil
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

// ConfirmPasswordReset redeems a one-shot token: revokes every live
// session, sets the new password, and issues a fresh login. It holds the
// account's credential-change claim throughout (claimCredentialChange), and
// claims the link itself before anything is revoked: a second submission of
// the same link finds it claimed and revokes nothing. A revocation that
// fails before the commit is answered as the failure it is and gives the
// link back, the password left as it was (the sessions may already have
// ended); the commit is followed by a second revocation and the new session
// (openSessionAfterChange), and a failure there is ErrPasswordChangedSignIn:
// the password is reset and the link spent. A commit that failed with
// anything but its precondition's refusal may have landed: the second
// revocation runs anyway and the link stays spent (commitUnconfirmed).
func (s *Service) ConfirmPasswordReset(ctx context.Context, token, newPassword, userAgent, ip string) (LoginResult, error) {
	if s.resets == nil {
		return LoginResult{}, ErrInvalidCredentials
	}
	if len(newPassword) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	rec, err := s.resets.GetByTokenHash(ctx, HashRefreshToken(token))
	if errors.Is(err, ErrResetNotFound) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: read the reset record: %w", err)
	}
	// Millisecond precision, the store's: Release matches the stamp exactly.
	now := s.now().UTC().Truncate(time.Millisecond)
	if now.After(rec.ExpiresAt) || rec.ConsumedAt != nil {
		// A spent link refuses here, before anything below: a replay of an
		// old link must not sign the account out everywhere.
		return LoginResult{}, ErrInvalidCredentials
	}
	// Every read runs before the link is claimed: a read that fails is
	// answered as the failure it is and leaves the link usable.
	u, err := s.store.GetUser(ctx, rec.UserID)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && u.Status == identity.UserStatusDisabled) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: read the account of reset %s: %w", rec.ID, err)
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return LoginResult{}, err
	}
	ctx, release, err := s.claimCredentialChange(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	defer release()
	lc, err := s.prepareLogin(ctx, u, "")
	if err != nil {
		return LoginResult{}, err
	}
	ok, err := s.resets.Consume(ctx, rec.ID, now)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: consume reset %s: %w", rec.ID, err)
	}
	if !ok {
		return LoginResult{}, ErrInvalidCredentials
	}
	// A reset means the old credential may be compromised: every live
	// session ends before the new password lands.
	if _, err := s.revokeAccount(ctx, u.ID); err != nil {
		s.releaseReset(ctx, rec.ID, now)
		return LoginResult{}, err
	}
	active := identity.UserStatusActive
	// The redemption owns the credential, the status it unblocks and the
	// lockout — under the precondition that a deactivation committed since
	// the check above wins: a token minted while the account was open must
	// not reactivate one an admin has closed since, nor hand out a session
	// for it. The refusal stays opaque, like the disabled check above.
	u, err = s.store.PatchUser(ctx, u.ID, identity.UserPatch{
		PasswordHash: &hash,
		Status:       &active,
		Lockout:      &identity.UserLockout{},
		LastLoginAt:  &now,
		IfNotStatus:  []identity.UserStatus{identity.UserStatusDisabled},
	})
	if errors.Is(err, identity.ErrUserStatusChanged) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, s.commitUnconfirmed(ctx, rec.UserID, err)
	}
	return s.openSessionAfterChange(ctx, lc, u, userAgent, ip)
}

// releaseReset gives a claimed reset link back after its confirm failed
// before its commit was sent. A release that fails is logged: the link
// stays spent, and the account's owner requests another one.
func (s *Service) releaseReset(ctx context.Context, id string, at time.Time) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionOpTimeout)
	defer cancel()
	if err := s.resets.Release(rctx, id, at); err != nil && s.logger != nil {
		s.logger.Error("auth: give back reset %s after its confirm failed: %v (the link stays spent)", id, err)
	}
}

// ChangePassword is the authenticated self-service rotation: verify the
// current password, revoke every session of the account, set the new
// password, and re-issue a login so the caller's session continues
// seamlessly. It holds the account's credential-change claim throughout
// (claimCredentialChange): a twin of the request waits, then finds the
// password it verified replaced. The revocation runs before the new password
// is committed: one that fails is answered as the failure it is and leaves
// the password as it was (the sessions may already have ended). The commit
// is followed by a second revocation and the new session
// (openSessionAfterChange); a failure there is ErrPasswordChangedSignIn.
// Personal access tokens are kept: a password change is no compromise
// response (RevokeUserSessions and the PAT revocation of "sign out
// everywhere" are).
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword, userAgent, ip string) (LoginResult, error) {
	if len(newPassword) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	u, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && u.Status != identity.UserStatusActive) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: read user %s: %w", userID, err)
	}
	// The current password is this route's proof of authority, the login's
	// proof's twin: it is refused while the account is locked and a wrong one
	// feeds the same brute-force counter the sign-in reads.
	if passwordProofLocked(s.now(), u) {
		_, _ = VerifyPassword(currentPassword, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(currentPassword, u.PasswordHash)
	if err != nil {
		return LoginResult{}, ErrInvalidCredentials
	}
	if !ok {
		s.countPasswordProofFailure(ctx, u)
		return LoginResult{}, ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return LoginResult{}, err
	}
	ctx, release, err := s.claimCredentialChange(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	defer release()
	verified := u.PasswordHash
	if u, err = s.store.GetUser(ctx, userID); err != nil {
		return LoginResult{}, fmt.Errorf("auth: read user %s: %w", userID, err)
	}
	if u.Status != identity.UserStatusActive || u.PasswordHash != verified {
		return LoginResult{}, ErrInvalidCredentials
	}
	// The session's reads run before the change commits: a read that fails
	// then leaves the password and every session as they were.
	lc, err := s.prepareLogin(ctx, u, "")
	if err != nil {
		return LoginResult{}, err
	}
	if _, err := s.revokeAccount(ctx, u.ID); err != nil {
		return LoginResult{}, err
	}
	// The rotation owns the credential alone, under the precondition that
	// REPRODUCES the checks above: the row must still be active — the new
	// password must not land on a closed account, nor over an admin password
	// reset committed since, whose temporary password is already on its way
	// to the account's owner out of band — and still hold the password this
	// request verified.
	u, err = s.store.PatchUser(ctx, u.ID, identity.UserPatch{
		PasswordHash: &hash,
		// The proof verified: the brute-force counter it fed clears, the
		// way a verified login clears it.
		Lockout: &identity.UserLockout{},
		IfNotStatus: []identity.UserStatus{
			identity.UserStatusDisabled,
			identity.UserStatusPendingPasswordChange,
		},
		IfPasswordHash: &verified,
	})
	if errors.Is(err, identity.ErrUserStatusChanged) || errors.Is(err, identity.ErrUserPasswordChanged) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, s.commitUnconfirmed(ctx, userID, err)
	}
	return s.openSessionAfterChange(ctx, lc, u, userAgent, ip)
}

// ResetPasswordByAdmin replaces the account's password with hash — a
// temporary one an administrator hands to its owner out of band — and marks
// the account pending_password_change, under the account's credential-change
// claim: every session ends before the commit, and again after it, so a
// sign-in with the old password during the reset is covered; personal access
// tokens are kept and refused while the account is pending. A refusal of the
// commit (the account disabled meanwhile) is identity.ErrUserStatusChanged;
// a commit that failed otherwise may have landed, and the revocation after it
// runs anyway (commitUnconfirmed). A revocation that fails before the commit
// is returned, nothing committed; one that fails after it is logged — the
// temporary password is set, and the pending status already refuses every
// session's refresh and derivation.
func (s *Service) ResetPasswordByAdmin(ctx context.Context, userID, hash string) (identity.User, error) {
	ctx, release, err := s.claimCredentialChange(ctx, userID)
	if err != nil {
		return identity.User{}, err
	}
	defer release()
	if _, err := s.revokeAccount(ctx, userID); err != nil {
		return identity.User{}, err
	}
	pending := identity.UserStatusPendingPasswordChange
	u, err := s.store.PatchUser(ctx, userID, identity.UserPatch{
		PasswordHash: &hash,
		Status:       &pending,
		IfNotStatus:  []identity.UserStatus{identity.UserStatusDisabled},
	})
	if errors.Is(err, identity.ErrUserStatusChanged) {
		return identity.User{}, err
	}
	if err != nil {
		return identity.User{}, s.commitUnconfirmed(ctx, userID, err)
	}
	if _, err := s.revokeAccount(ctx, userID); err != nil && s.logger != nil {
		s.logger.Error("auth: the temporary password of user %s is set, but the revocation after it failed: %v", userID, err)
	}
	return u, nil
}

// logf logs through the service logger when one is wired.
func (s *Service) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}
