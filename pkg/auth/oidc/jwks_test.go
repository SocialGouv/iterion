package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	return k
}

func rsaJWK(kid string, pub *rsa.PublicKey) jwk {
	eb := big.NewInt(int64(pub.E)).Bytes()
	return jwk{
		Kty: "RSA", Kid: kid,
		N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(eb),
	}
}

// jwksTestServer serves a JWKS document with the given key(s).
func jwksTestServer(t *testing.T, keys ...jwk) *jwksCache {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwkSet{Keys: keys})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return newJWKSCache(ts.URL+"/jwks", ts.Client())
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func idClaims(iss, aud, sub string, exp time.Time) idTokenClaims {
	return idTokenClaims{
		Subject: sub, Email: "u@acme.example", EmailVerified: true,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    iss,
			Audience:  jwt.ClaimStrings{aud},
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		},
	}
}

func TestParseJWK_RSARoundTrip(t *testing.T) {
	key := testRSAKey(t)
	pub, err := parseJWK(rsaJWK("k1", &key.PublicKey))
	if err != nil {
		t.Fatalf("parseJWK: %v", err)
	}
	got, ok := pub.(*rsa.PublicKey)
	if !ok || got.N.Cmp(key.N) != 0 || got.E != key.E {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestVerifyIDToken_HappyPath(t *testing.T) {
	key := testRSAKey(t)
	cache := jwksTestServer(t, rsaJWK("k1", &key.PublicKey))
	token := signRS256(t, key, "k1", idClaims("https://iss.example", "client-1", "sub-1", time.Now().Add(time.Hour)))
	claims, err := verifyIDToken(context.Background(), cache, token, "https://iss.example", "client-1")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "sub-1" || !claims.EmailVerified {
		t.Errorf("claims = %+v", claims)
	}
}

func TestVerifyIDToken_Rejections(t *testing.T) {
	key := testRSAKey(t)
	otherKey := testRSAKey(t)
	cache := jwksTestServer(t, rsaJWK("k1", &key.PublicKey))
	ctx := context.Background()
	good := idClaims("https://iss.example", "client-1", "sub-1", time.Now().Add(time.Hour))

	t.Run("bad signature (wrong key)", func(t *testing.T) {
		tok := signRS256(t, otherKey, "k1", good) // signed by a key not in the JWKS
		if _, err := verifyIDToken(ctx, cache, tok, "https://iss.example", "client-1"); err == nil {
			t.Fatal("expected bad-signature rejection")
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		tok := signRS256(t, key, "k1", idClaims("https://evil.example", "client-1", "s", time.Now().Add(time.Hour)))
		if _, err := verifyIDToken(ctx, cache, tok, "https://iss.example", "client-1"); err == nil {
			t.Fatal("expected wrong-issuer rejection")
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		tok := signRS256(t, key, "k1", idClaims("https://iss.example", "other-client", "s", time.Now().Add(time.Hour)))
		if _, err := verifyIDToken(ctx, cache, tok, "https://iss.example", "client-1"); err == nil {
			t.Fatal("expected wrong-audience rejection")
		}
	})
	t.Run("expired", func(t *testing.T) {
		tok := signRS256(t, key, "k1", idClaims("https://iss.example", "client-1", "s", time.Now().Add(-time.Hour)))
		if _, err := verifyIDToken(ctx, cache, tok, "https://iss.example", "client-1"); err == nil {
			t.Fatal("expected expired rejection")
		}
	})
	t.Run("alg none", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, good)
		tok.Header["kid"] = "k1"
		s, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if _, err := verifyIDToken(ctx, cache, s, "https://iss.example", "client-1"); err == nil {
			t.Fatal("expected alg=none rejection")
		}
	})
	t.Run("alg confusion HS256 with public key", func(t *testing.T) {
		// Classic attack: HS256 token using the RSA public key bytes as the
		// HMAC secret. The method allowlist must reject HS256 outright.
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, good)
		tok.Header["kid"] = "k1"
		s, _ := tok.SignedString(key.N.Bytes())
		if _, err := verifyIDToken(ctx, cache, s, "https://iss.example", "client-1"); err == nil {
			t.Fatal("expected HS256 rejection (alg confusion)")
		}
	})
}

// TestGenericConnector_VerifiesIDToken drives the full connector against a fake
// OIDC provider that returns a signed id_token + serves a jwks_uri, in strict
// mode (id_token required).
func TestGenericConnector_VerifiesIDToken(t *testing.T) {
	key := testRSAKey(t)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"userinfo_endpoint":      base + "/userinfo",
			"jwks_uri":               base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwkSet{Keys: []jwk{rsaJWK("k1", &key.PublicKey)}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		idt := signRS256(t, key, "k1", idClaims(base, "client", "kc-sub", time.Now().Add(time.Hour)))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "kc-sub", "email": "u@acme.example", "email_verified": true, "name": "U",
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	base = ts.URL

	// strict=false + http loopback (strict's https-endpoint check would reject
	// the test server's http endpoints); id_token verification runs regardless
	// of strict (verify-if-present).
	c := NewGenericConnectorWithSlug("oidc-org-x", ts.URL, "client", "secret", "Acme", nil, ts.Client(), false)
	ext, err := c.ExchangeCode(context.Background(), "code", "https://app/cb", "verifier")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if ext.Subject != "kc-sub" || ext.Email != "u@acme.example" {
		t.Errorf("ext = %+v", ext)
	}
}

func TestGenericConnector_StrictRequiresIDToken(t *testing.T) {
	// A strict connector whose token response omits the id_token is rejected.
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": base, "authorization_endpoint": base + "/a",
			"token_endpoint": base + "/token", "userinfo_endpoint": base + "/u",
			"jwks_uri": base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwkSet{Keys: []jwk{}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer"}) // no id_token
	})
	mux.HandleFunc("/u", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "s", "email": "e@x", "email_verified": true})
	})
	// TLS server so the strict https-endpoint check passes and we reach the
	// "id_token required" rejection (the point of this test).
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	base = ts.URL
	c := NewGenericConnectorWithSlug("oidc-org-x", ts.URL, "client", "secret", "Acme", nil, ts.Client(), true)
	if _, err := c.ExchangeCode(context.Background(), "code", "https://app/cb", "v"); err == nil {
		t.Fatal("expected strict connector to reject a response with no id_token")
	} else if !strings.Contains(err.Error(), "no id_token") {
		t.Errorf("unexpected error: %v", err)
	}
}
