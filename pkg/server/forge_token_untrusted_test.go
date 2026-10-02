package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// forge_token reaches a run by TWO carriers, and the sealed-bundle
// withdrawal only covers one. The second is the secret id the launch pinned
// on the run document (Run.SecretOverrides["forge_token"]), which the SERVER
// unseals at merge time through this resolver — no bundle involved.
//
// It is reached from pkg/runview/service_control.go for every repo-targeted
// merge, and what it enables is worse than reading: the merge clones the
// TENANT's repository with that token and pushes the run's branch into the
// merge target. On a fork-lane run that would push a contributor's tree into
// the tenant's own branch under the tenant's forge identity.
func TestForgeTokenForRun_RefusesAnUntrustedRun(t *testing.T) {
	s := &Server{}
	pinned := map[string]string{"forge_token": "sec-tenant-forge"}

	// The witness: a trusted run gets PAST the trust check and fails later,
	// on the wiring this bare fixture does not have. That is what proves the
	// refusal below is about trust and not about an unconfigured server.
	_, err := s.forgeTokenForRun(context.Background(), &store.Run{
		ID: "run-trusted", TenantID: "t1", RepoURL: "https://github.com/t/r.git", SecretOverrides: pinned,
	})
	if err == nil || !strings.Contains(err.Error(), "forge integrations are not wired") {
		t.Fatalf("trusted run: err = %v, want the later not-wired failure — if it fails earlier, this fixture cannot witness a trust refusal", err)
	}

	for _, trust := range []store.RunTrust{store.RunTrustFork, store.RunTrust("vendored")} {
		tok, err := s.forgeTokenForRun(context.Background(), &store.Run{
			ID: "run-untrusted", TenantID: "t1", RepoURL: "https://github.com/t/r.git",
			SecretOverrides: pinned, Trust: trust,
		})
		if err == nil {
			t.Fatalf("trust=%q: err = nil, want a refusal — the tenant's forge token must never be opened for a workspace it did not write", trust)
		}
		if tok != "" {
			t.Fatalf("trust=%q: returned a token (%d bytes) alongside its error", trust, len(tok))
		}
		if !strings.Contains(err.Error(), "untrusted workspace") {
			t.Fatalf("trust=%q: err = %v, want it to name the untrusted workspace so an operator is not sent hunting a credential bug", trust, err)
		}
	}
}
