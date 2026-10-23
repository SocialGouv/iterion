package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/server"
)

// membershipUnreadable is an identity store whose membership read fails —
// one of the four rows the server's PAT middleware resolves on every
// authenticated request. It is armed only once the account exists, so the
// login and the mint go through and the failure lands exactly where
// `iterion remote login --email` checks the token it just minted.
type membershipUnreadable struct {
	identity.Store
	armed bool
}

func (s *membershipUnreadable) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if s.armed {
		return identity.Membership{}, fmt.Errorf("identity: GetMembership: %w",
			fmt.Errorf("server selection error: context deadline exceeded, current topology: { Type: Unknown, Servers: [{ Addr: iterion-mongo-0.iterion-mongo-headless.iterion-prod.svc.cluster.local:27017 }] }"))
	}
	return s.Store.GetMembership(ctx, userID, teamID)
}

// realIterionServer starts the actual pkg/server HTTP surface with auth and
// personal access tokens enabled, holding one account with a personal team,
// and returns its base URL plus the store wrapper a test arms.
func realIterionServer(t *testing.T, email, password string) (string, *membershipUnreadable) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	store := &membershipUnreadable{Store: identity.NewMemoryStore()}
	svc, err := auth.NewService(auth.Config{
		Store: store, Sessions: auth.NewMemorySessionStore(), Signer: signer,
		SignupMode: auth.SignupOpen, RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	if _, err := svc.Register(context.Background(), email, password, "", "", "ua", "ip"); err != nil {
		t.Fatalf("register the account: %v", err)
	}
	s := server.New(server.Config{
		WorkDir: t.TempDir(), Bind: "127.0.0.1", SkipProjectRegistration: true,
		AuthService: svc, AuthSigner: signer, PATs: pat.NewMemoryStore(),
	}, iterlog.New(iterlog.LevelError, nil))
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return hs.URL, store
}

// TestRemoteLogin_AgainstARealServerWhoseIdentityStoreFails is the composed
// half of TestRemoteLogin_KeepsATokenTheServerDidNotReject: that test proves
// the CLI keeps a token when /api/auth/me answers 500, this one proves the
// REAL server answers 500 — and not 401 — when a row its PAT middleware
// needs cannot be read. Measured apart, each half passed while the pair was
// broken: the server answered the middleware's failed reads 401 with the
// driver's text, so the CLI read "the token was rejected", printed the
// identity cluster's hostnames, and threw away a token it had just made the
// server mint, leaving its row behind.
func TestRemoteLogin_AgainstARealServerWhoseIdentityStoreFails(t *testing.T) {
	const email, password = "ops@example.test", "password-forever-9"
	for _, tc := range []struct {
		name  string
		armed bool
	}{
		{"the store answering", false},
		{"a membership read the store cannot complete", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, store := realIterionServer(t, email, password)
			store.armed = tc.armed
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ITERION_TOKEN", "")
			t.Setenv("ITERION_PASSWORD", "")
			for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG"} {
				t.Setenv(env, "")
			}
			prevEmail, prevPassword, prevToken := remoteEmail, remotePassword, remoteToken
			t.Cleanup(func() { remoteEmail, remotePassword, remoteToken = prevEmail, prevPassword, prevToken })
			remoteEmail, remotePassword, remoteToken = email, password, ""
			remoteLoginCmd.SetContext(context.Background())

			var err error
			out := captureStdoutAllowingError(t, func() error {
				err = runRemoteLogin(remoteLoginCmd, []string{base})
				return err
			})
			if err != nil {
				t.Fatalf("the login against a real server with %s: %v (stdout %q)", tc.name, err, out)
			}
			saved, loadErr := cli.LoadRemoteConfig()
			if loadErr != nil || saved.Token == "" {
				t.Fatalf("the minted token was not saved: %+v (err %v)", saved, loadErr)
			}
			if saved.BaseURL != base {
				t.Fatalf("the saved base URL is %q, want %q", saved.BaseURL, base)
			}
			// Nothing the server knows about its own internals reaches the
			// terminal: a store failure's text names the identity cluster.
			if strings.Contains(out, "iterion-mongo-0") {
				t.Fatalf("the CLI printed the store's internal hosts: %q", out)
			}
		})
	}
}
