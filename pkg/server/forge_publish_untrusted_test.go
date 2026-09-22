package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The publish grant is the capability to post a review, a comment and the
// revi/review commit status that GATES the merge. A run whose workspace holds
// code the tenant did not write must never hold one.
//
// The grant is minted BEFORE the run document exists (launchWebhookTarget
// calls this ~25 lines ahead of the launcher), so this control cannot read a
// run: trust is a parameter here, and runOwnsGrant is the twin on the other
// side of the run's creation.
func TestInjectForgePublishVars_RefusesAnUntrustedLaunch(t *testing.T) {
	prVars := func() map[string]string {
		return map[string]string{"pr_url": "https://github.com/o/r/pull/42"}
	}

	// The trusted arm is the fixture's own witness: the same server, the same
	// connection, the same PR — a grant IS available here. Without it a
	// refusal below would be indistinguishable from "no connection covers
	// this repo", which is a different answer entirely.
	t.Run("a trusted launch is minted a grant", func(t *testing.T) {
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.example"
		out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", prVars(), nil, store.RunTrustDefault)
		if err != nil {
			t.Fatalf("injectForgePublishVars = %v, want nil", err)
		}
		if out[forgePublishVarToken] == "" {
			t.Fatal("no grant minted for a trusted launch — the fixture cannot witness a refusal it could not have granted")
		}
	})

	t.Run("a fork-trust launch is refused, by name", func(t *testing.T) {
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.example"
		out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", prVars(), nil, store.RunTrustFork)
		if !errors.Is(err, errForgePublishGrantUntrusted) {
			t.Fatalf("injectForgePublishVars = %v, want errForgePublishGrantUntrusted — the refusal has to be an error, not a quiet skip, or a capability going missing is indistinguishable from a misconfiguration", err)
		}
		if tok := out[forgePublishVarToken]; tok != "" {
			t.Fatalf("token var = %q on a refused launch, want empty", tok)
		}
	})

	// Trusted(), not "== fork": an unrecognised trust must lose the grant.
	t.Run("an unrecognised trust is refused too", func(t *testing.T) {
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.example"
		_, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", prVars(), nil, store.RunTrust("vendored"))
		if !errors.Is(err, errForgePublishGrantUntrusted) {
			t.Fatalf("injectForgePublishVars = %v, want errForgePublishGrantUntrusted for a trust this binary does not know", err)
		}
	})

	// The strip sits ahead of BOTH early returns. This one is the no-pr_url
	// return: a launch that names no pull request used to hand the vars back
	// untouched, carrying whatever grant the caller had put in them.
	t.Run("with no pr_url a pinned grant is still withdrawn", func(t *testing.T) {
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.example"
		vars := map[string]string{"base_ref": "main", forgePublishVarToken: "pinned-grant-token"}
		out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", vars, nil, store.RunTrustFork)
		if err != nil {
			t.Fatalf("injectForgePublishVars = %v, want nil — nothing was going to be minted, so an untrusted launch with no PR still launches", err)
		}
		if got, ok := out[forgePublishVarToken]; ok {
			t.Fatalf("forge_publish_token = %q survived an untrusted launch that named no pull request", got)
		}
	})

	// The withdrawal must cover the COMPLETE set the mint writes. It was
	// first written as three literal deletes against a mint of four.
	t.Run("every var the mint writes is withdrawn", func(t *testing.T) {
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.example"
		minted, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", prVars(), nil, store.RunTrustDefault)
		if err != nil {
			t.Fatalf("trusted mint = %v, want nil", err)
		}
		// Derived from what the mint ACTUALLY produced, not from a list
		// copied here: a hand-copied list drifts the same way the strip did.
		var mintedKeys []string
		for k := range minted {
			if strings.HasPrefix(k, "forge_publish") || strings.HasPrefix(k, "forge_pr_state") || strings.HasPrefix(k, "forge_delivery") {
				mintedKeys = append(mintedKeys, k)
			}
		}
		if len(mintedKeys) < 4 {
			t.Fatalf("the trusted mint produced %v — expected at least 4 grant vars, so this test would not notice one surviving", mintedKeys)
		}
		carried := prVars()
		for _, k := range mintedKeys {
			carried[k] = minted[k]
		}
		out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", carried, nil, store.RunTrustFork)
		if !errors.Is(err, errForgePublishGrantUntrusted) {
			t.Fatalf("injectForgePublishVars = %v, want errForgePublishGrantUntrusted", err)
		}
		for _, k := range mintedKeys {
			if got, ok := out[k]; ok {
				t.Fatalf("%s = %q survived on an untrusted launch — the withdrawal must cover every var the mint writes", k, got)
			}
		}
	})

	// The other early return. The forbidden alternative here is not "no grant
	// is minted" — it is "a grant the CALLER supplied survives": the pin
	// branch hands its vars straight back, so a check placed after it would
	// let any lane arm an untrusted run by putting a token in vars.
	t.Run("a caller-PINNED token is stripped, not honoured", func(t *testing.T) {
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.example"
		vars := prVars()
		vars[forgePublishVarToken] = "pinned-by-the-caller"
		vars[forgePublishVarURL] = "https://iterion.example/api/v1/forge/publish-review"
		vars[forgePublishVarPRState] = "https://iterion.example/api/v1/forge/pull-request"
		out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", vars, nil, store.RunTrustFork)
		if !errors.Is(err, errForgePublishGrantUntrusted) {
			t.Fatalf("injectForgePublishVars = %v, want errForgePublishGrantUntrusted", err)
		}
		for _, k := range []string{forgePublishVarToken, forgePublishVarURL, forgePublishVarPRState} {
			if got, ok := out[k]; ok {
				t.Fatalf("%s = %q survived on an untrusted launch — a pinned publish var must be stripped, not carried", k, got)
			}
		}
	})
}

// runOwnsGrant is the belt to injectForgePublishVars' braces: it refuses to
// HONOUR a grant an untrusted run presents, on the far side of the run's
// creation, so no single mistake clears both. The tenant comparison alone
// cannot do this — a fork-lane run and a trusted run of the SAME tenant are
// identical to it.
func TestRunOwnsGrant_RefusesAnUntrustedRun(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	grant := ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}

	// Same tenant on both sides: the tenant check says yes, so anything that
	// refuses below is refusing on trust and on nothing else.
	if !s.runOwnsGrant(&store.Run{ID: "run-ok", TenantID: "team1"}, grant, "publish-review") {
		t.Fatal("a trusted run of the grant's own tenant was refused — the fixture proves nothing about trust")
	}
	if s.runOwnsGrant(&store.Run{ID: "run-fork", TenantID: "team1", Trust: store.RunTrustFork}, grant, "publish-review") {
		t.Fatal("an untrusted run was allowed to present its own team's grant — the tenant comparison cannot tell the two apart, which is why the trust check comes first")
	}
	if s.runOwnsGrant(&store.Run{ID: "run-unknown", TenantID: "team1", Trust: store.RunTrust("vendored")}, grant, "publish-review") {
		t.Fatal("a run whose trust this binary does not recognise was allowed to publish — an unknown trust must fail closed")
	}
}
