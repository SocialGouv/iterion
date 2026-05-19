package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// randomKey returns a 32-byte base64-encoded secret suitable for HS256.
func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.RawStdEncoding.EncodeToString(b)
}

func newSignerForTest(t *testing.T, ttl time.Duration) *JWTSigner {
	t.Helper()
	s, err := NewJWTSigner(randomKey(t), ttl)
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	return s
}

// ---- key construction --------------------------------------------------

func TestNewJWTSigner_RejectsShortSecret(t *testing.T) {
	short := base64.RawStdEncoding.EncodeToString([]byte("only-16-bytes-x!"))
	_, err := NewJWTSigner(short, time.Minute)
	if err == nil {
		t.Fatal("expected error on short secret")
	}
}

func TestNewJWTSigner_DefaultsAccessTTLWhenZero(t *testing.T) {
	s, err := NewJWTSigner(randomKey(t), 0)
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	if s.AccessTTL() != 15*time.Minute {
		t.Errorf("expected 15m default, got %s", s.AccessTTL())
	}
}

func TestNewJWTSignerMulti_DefaultsActiveKIDToLargestLex(t *testing.T) {
	a := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	b := JWTKey{ID: "k2", Secret: make([]byte, 32)}
	c := JWTKey{ID: "k1", Secret: make([]byte, 32)}
	_, _ = rand.Read(a.Secret)
	_, _ = rand.Read(b.Secret)
	_, _ = rand.Read(c.Secret)
	s, err := NewJWTSignerMulti([]JWTKey{a, b, c}, "", time.Minute, nil)
	if err != nil {
		t.Fatalf("NewJWTSignerMulti: %v", err)
	}
	if s.ActiveKID() != "k2" {
		t.Errorf("expected active=k2, got %s", s.ActiveKID())
	}
}

func TestNewJWTSignerMulti_RejectsMissingActiveKID(t *testing.T) {
	a := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	_, err := NewJWTSignerMulti([]JWTKey{a}, "k99", time.Minute, nil)
	if err == nil || !strings.Contains(err.Error(), "not in key set") {
		t.Errorf("expected active-kid mismatch err, got %v", err)
	}
}

func TestNewJWTSignerMulti_RejectsEmptyAndDuplicateKIDs(t *testing.T) {
	a := JWTKey{ID: "", Secret: make([]byte, 32)}
	if _, err := NewJWTSignerMulti([]JWTKey{a}, "", time.Minute, nil); err == nil {
		t.Error("expected error on empty ID")
	}
	b := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	dup := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	if _, err := NewJWTSignerMulti([]JWTKey{b, dup}, "k0", time.Minute, nil); err == nil {
		t.Error("expected error on duplicate ID")
	}
}

func TestNewJWTSignerMulti_RejectsShortSecret(t *testing.T) {
	bad := JWTKey{ID: "k0", Secret: []byte("only-15-bytesxxx")[:15]}
	if _, err := NewJWTSignerMulti([]JWTKey{bad}, "k0", time.Minute, nil); err == nil {
		t.Error("expected error on short key")
	}
}

// ---- happy round-trip --------------------------------------------------

func TestIssueAndVerify_Roundtrip(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	id := Identity{
		UserID: "u-1",
		Email:  "alice@example.com",
		TeamID: "t-1",
		Role:   identity.RoleAdmin,
	}
	token, exp, err := signer.IssueAccess(id)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if token == "" || exp.IsZero() {
		t.Fatal("empty token / exp")
	}
	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.UserID != id.UserID || got.Email != id.Email || got.TeamID != id.TeamID || got.Role != id.Role {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, id)
	}
	if got.JTI == "" {
		t.Error("expected JTI to be populated")
	}
}

// ---- expiry ------------------------------------------------------------

func TestVerify_RejectsExpired(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	// Backdate "now" so the issued token is already expired by the
	// time Verify checks the exp claim.
	signer.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	token, _, err := signer.IssueAccess(Identity{UserID: "u"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	signer.now = time.Now
	_, err = signer.Verify(token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

// ---- tamper detection --------------------------------------------------

func TestVerify_RejectsSignatureTamper(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	token, _, err := signer.IssueAccess(Identity{UserID: "u"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Flip a byte in the signature.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed token: %d parts", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "." + parts[2] + "x"
	_, err = signer.Verify(tampered)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestVerify_RejectsAlgNone(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	// Forge a token using alg=none. golang-jwt blocks this via
	// jwt.WithValidMethods.
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u",
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tk := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	raw, err := tk.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("forge: %v", err)
	}
	_, err = signer.Verify(raw)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid on alg=none, got %v", err)
	}
}

func TestVerify_RejectsWrongIssuer(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	// Issue with the right setup, then mint a hand-rolled token with
	// the wrong iss using a known secret to bypass the issuer.
	key := signer.keys["k0"].Secret
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u",
			Issuer:    "not-iterion",
			Audience:  jwt.ClaimStrings{jwtAudience},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tk := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	raw, err := tk.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = signer.Verify(raw)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid on wrong iss, got %v", err)
	}
}

func TestVerify_RejectsWrongAudience(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	key := signer.keys["k0"].Secret
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u",
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{"other-aud"},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tk := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	raw, err := tk.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = signer.Verify(raw)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid on wrong aud, got %v", err)
	}
}

// ---- kid rotation ------------------------------------------------------

func TestVerify_AcceptsTokenFromOldKeyAfterRotation(t *testing.T) {
	k0 := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	_, _ = rand.Read(k0.Secret)
	signer, err := NewJWTSignerMulti([]JWTKey{k0}, "k0", time.Hour, nil)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	oldToken, _, err := signer.IssueAccess(Identity{UserID: "u"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Add k1 and make it active. k0 stays in the map so prior tokens verify.
	k1 := JWTKey{ID: "k1", Secret: make([]byte, 32)}
	_, _ = rand.Read(k1.Secret)
	rotated, err := NewJWTSignerMulti([]JWTKey{k0, k1}, "k1", time.Hour, nil)
	if err != nil {
		t.Fatalf("rotated: %v", err)
	}

	got, err := rotated.Verify(oldToken)
	if err != nil {
		t.Fatalf("verify old token after rotation: %v", err)
	}
	if got.UserID != "u" {
		t.Errorf("unexpected identity: %+v", got)
	}
}

func TestVerify_RejectsUnknownKID(t *testing.T) {
	k0 := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	_, _ = rand.Read(k0.Secret)
	signerA, _ := NewJWTSignerMulti([]JWTKey{k0}, "k0", time.Hour, nil)
	token, _, _ := signerA.IssueAccess(Identity{UserID: "u"})

	// Build a fresh signer that does NOT know k0 — its only key is k99.
	k99 := JWTKey{ID: "k99", Secret: make([]byte, 32)}
	_, _ = rand.Read(k99.Secret)
	signerB, _ := NewJWTSignerMulti([]JWTKey{k99}, "k99", time.Hour, nil)

	_, err := signerB.Verify(token)
	if err == nil {
		t.Error("expected error on unknown kid, got nil")
	}
	// Unknown kid currently falls through to the generic wrapped-error
	// path in Verify (not ErrTokenInvalid). Pin the substring so a
	// future refactor toward a typed error is caught.
	if err != nil && !strings.Contains(err.Error(), "unknown kid") {
		t.Errorf("expected 'unknown kid' in err, got %v", err)
	}
}

// ---- denylist ----------------------------------------------------------

type onceDenylist struct{ jti string }

func (d *onceDenylist) IsDenied(jti string) bool { return jti == d.jti }

func TestVerify_RespectsDenylist(t *testing.T) {
	k := JWTKey{ID: "k0", Secret: make([]byte, 32)}
	_, _ = rand.Read(k.Secret)
	deny := &onceDenylist{}
	signer, err := NewJWTSignerMulti([]JWTKey{k}, "k0", time.Hour, deny)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	token, _, err := signer.IssueAccess(Identity{UserID: "u"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Initially not denied.
	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	deny.jti = got.JTI
	// Now denied.
	_, err = signer.Verify(token)
	if !errors.Is(err, ErrTokenRevoked) {
		t.Errorf("expected ErrTokenRevoked after denylist hit, got %v", err)
	}
}

// ---- malformed inputs --------------------------------------------------

func TestVerify_RejectsMalformedToken(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	_, err := signer.Verify("not.a.jwt")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestVerify_RejectsMissingExp(t *testing.T) {
	signer := newSignerForTest(t, time.Hour)
	key := signer.keys["k0"].Secret
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "u",
			Issuer:   jwtIssuer,
			Audience: jwt.ClaimStrings{jwtAudience},
			IssuedAt: jwt.NewNumericDate(time.Now()),
			// no ExpiresAt → WithExpirationRequired rejects.
		},
	}
	tk := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	raw, err := tk.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = signer.Verify(raw)
	if err == nil {
		t.Error("expected error for token without exp")
	}
}
