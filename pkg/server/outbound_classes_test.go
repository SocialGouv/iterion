package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// The server's outbound callers decide the shared (CGNAT) space and a
// non-public IPv6 transition address by outboundStrict: in cloud mode the
// forge client and a per-org OIDC connector refuse them, each through its
// production construction; a loopback-bound local server's forge client
// admits them, read past the guard on a dial whose context is already
// canceled, so no packet leaves.
func TestOutboundClientsDecideTheSharedAndTransitionClasses(t *testing.T) {
	targets := []string{"100.64.0.1:9", "[64:ff9b::7f00:1]:9"}
	logger := iterlog.New(iterlog.LevelError, nil)

	cloud := New(Config{}, logger)
	cloud.cfg.Mode = "cloud"
	for _, target := range targets {
		if _, err := cloud.forgeHTTPClient().Get("http://" + target + "/api/v1/user"); !errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("cloud forge client: %s = %v; want the guard's refusal", target, err)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	cloud.sealer = sealer
	sealed, err := orgsso.SealClientSecret(sealer, "p-classes", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		conn, err := cloud.buildOrgOIDCConnector(orgsso.OrgSSOProvider{ID: "p-classes", Kind: orgsso.KindOIDC, Enabled: true, IssuerURL: "http://" + target, ClientID: "c", SealedSecret: sealed})
		if err != nil {
			t.Fatalf("build the per-org connector: %v", err)
		}
		if _, err := conn.AuthorizeURL(context.Background(), "https://iterion.test/cb", "st4te", "verifier"); !errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("cloud per-org OIDC discovery: %s = %v; want the guard's refusal", target, err)
		}
	}

	local := New(Config{}, logger)
	if local.outboundStrict() {
		t.Fatal("scenario broken: a default local server is loopback-bound, so its outbound guard is lifted")
	}
	tr, ok := local.forgeHTTPClient().Transport.(countingTransport).base.(*http.Transport)
	if !ok {
		t.Fatalf("the forge client's base transport is %T; want the guarded *http.Transport", local.forgeHTTPClient().Transport.(countingTransport).base)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, target := range targets {
		if _, err := tr.DialContext(canceled, "tcp", target); !errors.Is(err, context.Canceled) || errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("local forge client: dial %s = %v; want the dialer's cancellation, past the guard", target, err)
		}
	}
}
