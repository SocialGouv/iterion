package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// captureMailer records sent messages for assertions.
type captureMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (c *captureMailer) Enabled() bool { return true }
func (c *captureMailer) Send(_ context.Context, m mail.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

func (c *captureMailer) last() (mail.Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return mail.Message{}, false
	}
	return c.sent[len(c.sent)-1], true
}

func newResetTestService(t *testing.T) (*Service, *captureMailer, identity.Store) {
	t.Helper()
	signer, err := NewJWTSigner("dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQ", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store := identity.NewMemoryStore()
	mailer := &captureMailer{}
	svc, err := NewService(Config{
		Store:      store,
		Sessions:   NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: SignupOpen,
		RefreshTTL: time.Hour,
		Resets:     NewMemoryPasswordResetStore(),
		Mailer:     mailer,
		PublicURL:  "https://iterion.example.org/",
	})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := HashPassword("old-password-123")
	if _, err := store.CreateUser(context.Background(), identity.User{
		ID: "u1", Email: "user@example.org", PasswordHash: hash, Status: identity.UserStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	return svc, mailer, store
}

// extractResetToken pulls the iar_ token out of the captured email.
func extractResetToken(t *testing.T, m mail.Message) string {
	t.Helper()
	i := strings.Index(m.TextBody, ResetTokenPrefix)
	if i < 0 {
		t.Fatalf("no reset token in email body: %q", m.TextBody)
	}
	tok := m.TextBody[i:]
	if j := strings.IndexAny(tok, " \n\r"); j > 0 {
		tok = tok[:j]
	}
	return tok
}

func TestPasswordResetFlow(t *testing.T) {
	svc, mailer, _ := newResetTestService(t)
	ctx := context.Background()

	if err := svc.RequestPasswordReset(ctx, "USER@example.org"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	msg, ok := mailer.last()
	if !ok {
		t.Fatal("no email sent for a known account")
	}
	if !strings.Contains(msg.TextBody, "https://iterion.example.org/auth/reset?token=") {
		t.Fatalf("reset URL malformed: %q", msg.TextBody)
	}
	token := extractResetToken(t, msg)

	t.Run("weak password rejected", func(t *testing.T) {
		if _, err := svc.ConfirmPasswordReset(ctx, token, "short", "ua", "ip"); err != ErrPasswordWeak {
			t.Fatalf("err = %v, want ErrPasswordWeak", err)
		}
	})

	t.Run("confirm sets password + logs in", func(t *testing.T) {
		res, err := svc.ConfirmPasswordReset(ctx, token, "new-password-456", "ua", "ip")
		if err != nil {
			t.Fatalf("ConfirmPasswordReset: %v", err)
		}
		if res.User.ID != "u1" || res.AccessToken == "" {
			t.Fatalf("login result = %+v", res)
		}
		if _, err := svc.Login(ctx, "user@example.org", "new-password-456", "ua", "ip"); err != nil {
			t.Fatalf("login with new password: %v", err)
		}
		if _, err := svc.Login(ctx, "user@example.org", "old-password-123", "ua", "ip"); err == nil {
			t.Fatal("old password still works")
		}
	})

	t.Run("token is one-shot", func(t *testing.T) {
		if _, err := svc.ConfirmPasswordReset(ctx, token, "another-password-789", "ua", "ip"); err == nil {
			t.Fatal("consumed token redeemed twice")
		}
	})
}

func TestPasswordResetAntiEnumeration(t *testing.T) {
	svc, mailer, _ := newResetTestService(t)
	ctx := context.Background()
	if err := svc.RequestPasswordReset(ctx, "ghost@example.org"); err != nil {
		t.Fatalf("unknown account must return nil, got %v", err)
	}
	if _, ok := mailer.last(); ok {
		t.Fatal("email sent for unknown account")
	}
	// Disabled account: same silence.
	hash, _ := HashPassword("x-password-123")
	_, _ = svc.store.CreateUser(ctx, identity.User{ID: "u2", Email: "off@example.org", PasswordHash: hash, Status: identity.UserStatusDisabled})
	if err := svc.RequestPasswordReset(ctx, "off@example.org"); err != nil {
		t.Fatalf("disabled account must return nil, got %v", err)
	}
	if _, ok := mailer.last(); ok {
		t.Fatal("email sent for disabled account")
	}
}

func TestPasswordResetExpiry(t *testing.T) {
	svc, mailer, _ := newResetTestService(t)
	ctx := context.Background()
	base := time.Now()
	svc.now = func() time.Time { return base }
	if err := svc.RequestPasswordReset(ctx, "user@example.org"); err != nil {
		t.Fatal(err)
	}
	msg, _ := mailer.last()
	token := extractResetToken(t, msg)
	svc.now = func() time.Time { return base.Add(ResetTokenTTL + time.Minute) }
	if _, err := svc.ConfirmPasswordReset(ctx, token, "new-password-456", "ua", "ip"); err == nil {
		t.Fatal("expired token redeemed")
	}
}

func TestChangePassword(t *testing.T) {
	svc, _, _ := newResetTestService(t)
	ctx := context.Background()

	t.Run("wrong current rejected", func(t *testing.T) {
		if _, err := svc.ChangePassword(ctx, "u1", "wrong", "new-password-456", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("rotation works + revokes sessions", func(t *testing.T) {
		// Open a session that must die on rotation.
		pre, err := svc.Login(ctx, "user@example.org", "old-password-123", "ua", "ip")
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.ChangePassword(ctx, "u1", "old-password-123", "new-password-456", "ua", "ip")
		if err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		if res.AccessToken == "" {
			t.Fatal("no fresh login issued")
		}
		if _, err := svc.Refresh(ctx, pre.RefreshToken, "ua", "ip"); err == nil {
			t.Fatal("pre-rotation refresh token survived")
		}
		if _, err := svc.Login(ctx, "user@example.org", "new-password-456", "ua", "ip"); err != nil {
			t.Fatalf("login with rotated password: %v", err)
		}
	})
}
