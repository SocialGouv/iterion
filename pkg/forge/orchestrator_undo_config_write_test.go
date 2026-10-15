package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// assertLiveSecretVerifies asserts the production predicate the inbound
// middleware applies to every delivery (middleware_webhook.go →
// webhooks.VerifyToken(presented, cfg.TokenHash)): the secret the FORGE holds
// still authenticates against the stored config. False means every delivery
// to that repo answers 401, and nothing reports it.
func assertLiveSecretVerifies(t *testing.T, o *Orchestrator, webhookID, liveSecret, what string) {
	t.Helper()
	cfg, err := o.Webhooks.Get(context.Background(), webhookID)
	if err != nil {
		t.Fatalf("%s: no stored webhook config at all: %v", what, err)
	}
	if !webhooks.VerifyToken(liveSecret, cfg.TokenHash) {
		t.Errorf("%s: the secret the forge's live hook signs with no longer authenticates against "+
			"the stored config (VerifyToken=false) — every delivery to this repo answers 401", what)
	}
}

func provisionBots(t *testing.T, o *Orchestrator, bots ...string) ProvisionResult {
	t.Helper()
	res, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: bots, ActorID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// failingHookAdmin fails the hook write with err, WITHOUT applying it: the
// forge-side hook keeps the URL and the secret it had. before, when set, runs
// just before the failure — the moment after which only the undo of the
// config write is left.
type failingHookAdmin struct {
	*fakeAdmin
	err    error
	before func()
}

func (a *failingHookAdmin) UpdateHook(context.Context, string, string, HookSpec) (HookHandle, error) {
	if a.before != nil {
		a.before()
	}
	return HookHandle{}, a.err
}

// appliedThenFailedAdmin APPLIES the hook spec and then fails to answer — a
// lost response: a proxy 502 on the way back, a client timeout. The
// forge-side hook carries the NEW url and the NEW secret; iterion sees only
// an error.
type appliedThenFailedAdmin struct {
	*fakeAdmin
	err error
}

func (a *appliedThenFailedAdmin) UpdateHook(ctx context.Context, repo, hookID string, spec HookSpec) (HookHandle, error) {
	if _, err := a.fakeAdmin.UpdateHook(ctx, repo, hookID, spec); err != nil {
		return HookHandle{}, err
	}
	return HookHandle{}, a.err
}

// TestReprovision_AdminForFailureRestoresTheStoredConfig: the step between
// the config write and the forge is building the admin client (an
// installation-token fetch, an unseal). It makes no forge call, so the hook
// still carries the previous secret — and the re-provision has already
// replaced the stored config with a fresh one.
func TestReprovision_AdminForFailureRestoresTheStoredConfig(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	res := provisionBots(t, o, "review-pr")
	liveSecret := fa.lastSecret
	assertLiveSecretVerifies(t, o, res.WebhookID, liveSecret, "precondition (a working repo)")

	o.AdminFor = func(context.Context, Connection) (Admin, error) {
		return nil, errors.New("gitlab: installation token: HTTP 503")
	}
	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	if err == nil {
		t.Fatal("expected the admin-client failure to surface")
	}
	if got := fa.lastSecret; got != liveSecret {
		t.Fatalf("the forge-side secret moved to %q — no forge call was made, so the premise is wrong", got)
	}
	assertLiveSecretVerifies(t, o, res.WebhookID, liveSecret, "after AdminFor failed on a re-provision")
}

// TestReprovision_ForgeRejectionRestoresTheStoredConfig: a 4xx is the forge
// REFUSING the write, so the hook it has is untouched and the previous config
// is the one that matches it. The refusal itself must keep reaching the
// route — a restored config does not change what the operator must fix.
func TestReprovision_ForgeRejectionRestoresTheStoredConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		assert func(t *testing.T, err error)
	}{
		{
			name: "403 — the token lost its hook-admin grant",
			err:  StatusErr("gitlab", "update hook", http.StatusForbidden),
			assert: func(t *testing.T, err error) {
				if !errors.Is(err, ErrForbidden) {
					t.Errorf("the forge's refusal must stay classifiable, got %T: %v", err, err)
				}
			},
		},
		{
			name: "422 — the address itself",
			err:  StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity),
			assert: func(t *testing.T, err error) {
				var refused *HookRefusedError
				if !errors.As(err, &refused) {
					t.Errorf("want *HookRefusedError, got %T: %v", err, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, fa, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			res := provisionBots(t, o, "review-pr")
			liveSecret := fa.lastSecret

			useAdmin(o, &failingHookAdmin{fakeAdmin: fa, err: tc.err})
			_, err := o.Provision(context.Background(), ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"revi-converse"}, ActorID: "u1",
			})
			if err == nil {
				t.Fatal("expected the refusal to surface")
			}
			tc.assert(t, err)
			assertLiveSecretVerifies(t, o, res.WebhookID, liveSecret, "after the forge refused the hook write")
		})
	}
}

// TestReprovision_IndeterminateHookFailureKeepsTheFreshConfig: a transport
// error, a timeout and a 5xx say NOTHING about what the forge did with the
// request. A hook write whose response was lost did land — the forge now
// holds the new URL and the new secret — and the stored config this
// re-provision wrote is the only one that matches it: restoring there is what
// breaks the repo. The answer says so and names the remedy.
func TestReprovision_IndeterminateHookFailureKeepsTheFreshConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a lost response", errors.New(`Put "https://gitlab.example.com/api/v4/projects/group%2Fapi/hooks/hook-1": context deadline exceeded`)},
		{"a cancelled call", context.Canceled},
		{"the forge fell over", StatusErr("gitlab", "update hook", http.StatusBadGateway)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, fa, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			res := provisionBots(t, o, "review-pr")

			// The write LANDS and the answer is lost: the strongest form of
			// the case, since the forge-side secret is then the new one.
			useAdmin(o, &appliedThenFailedAdmin{fakeAdmin: fa, err: tc.err})
			_, err := o.Provision(context.Background(), ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"revi-converse"}, ActorID: "u1",
			})
			if err == nil {
				t.Fatal("expected the failure to surface")
			}
			assertLiveSecretVerifies(t, o, res.WebhookID, fa.lastSecret,
				"after a hook write that landed and lost its answer")
			var refused *HookRefusedError
			if errors.As(err, &refused) {
				t.Errorf("an indeterminate failure must not be typed as a refused address: %v", err)
			}
			var indeterminate *HookWriteIndeterminateError
			if !errors.As(err, &indeterminate) {
				t.Errorf("want *HookWriteIndeterminateError, got %T: %v", err, err)
			} else if indeterminate.WebhookID != res.WebhookID {
				t.Errorf("WebhookID = %q, want the repo's %q", indeterminate.WebhookID, res.WebhookID)
			}
			if !strings.Contains(err.Error(), "re-run the same request") {
				t.Errorf("the error does not name the remedy: %v", err)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("the forge's own failure is no longer reachable: %v", err)
			}
		})
	}
}

// gatedAdmin runs before() at the start of each hook write, so a test can
// order two provisions precisely.
type gatedAdmin struct {
	*fakeAdmin
	before    func()
	updateErr error
}

func (g *gatedAdmin) UpdateHook(ctx context.Context, repo, hookID string, spec HookSpec) (HookHandle, error) {
	g.before()
	if g.updateErr != nil {
		return HookHandle{}, g.updateErr
	}
	return g.fakeAdmin.UpdateHook(ctx, repo, hookID, spec)
}

// signalOnFirstUpdate closes fired after the first Update it forwards.
type signalOnFirstUpdate struct {
	webhooks.ConfigStore
	once  sync.Once
	fired chan struct{}
}

func (s *signalOnFirstUpdate) Update(ctx context.Context, c webhooks.Config) error {
	err := s.ConfigStore.Update(ctx, c)
	s.once.Do(func() { close(s.fired) })
	return err
}

// TestReprovision_RestoreDoesNotClobberAConcurrentWinner: the operator's
// pin-and-retry while the first call is still in flight on a slow forge. A
// replaces the config and calls the forge; B replaces the config, moves the
// forge hook to the pinned URL and succeeds — the repo WORKS; A comes back
// refused. A's snapshot predates all of it, so writing it back would revert
// the winner's config while the forge signs with the winner's secret.
func TestReprovision_RestoreDoesNotClobberAConcurrentWinner(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer)
	ctx := context.Background()
	res := provisionBots(t, o, "review-pr")

	wroteA := make(chan struct{})
	gate := make(chan struct{})
	o.Webhooks = &signalOnFirstUpdate{ConfigStore: o.Webhooks, fired: wroteA}
	slow := &gatedAdmin{fakeAdmin: fa, before: func() { <-gate },
		updateErr: StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity)}

	done := make(chan error, 1)
	loser := *o // the same stores and the same (still unpinned) connection
	loser.AdminFor = func(context.Context, Connection) (Admin, error) { return slow, nil }
	go func() {
		_, err := loser.Provision(ctx, ProvisionRequest{
			TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"revi-converse"}, ActorID: "u1",
		})
		done <- err
	}()
	<-wroteA // A replaced the stored config and is about to call the forge

	conn.WebhookBaseURL = "https://iterion.allowlisted.example" // the operator pins
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse", "dep-guard"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("B (the pinned re-enable) must succeed: %v", err)
	}
	winnerSecret := fa.lastSecret
	winnerURL := hookURLFor(t, fa, "group/api")
	assertLiveSecretVerifies(t, o, res.WebhookID, winnerSecret, "after B succeeded (the repo works)")

	close(gate)
	if err := <-done; err == nil {
		t.Fatal("A must fail: its hook URL is the refused one")
	}
	if got := hookURLFor(t, fa, "group/api"); got != winnerURL {
		t.Fatalf("the forge hook moved to %q — the premise is wrong", got)
	}
	assertLiveSecretVerifies(t, o, res.WebhookID, winnerSecret, "after the loser's restore")
}

// rereadFailStore fails the config re-read the restore compares against —
// armed once the provision reaches the forge, so the reads it makes on the
// way there succeed — while every other read and write succeeds.
type rereadFailStore struct {
	webhooks.ConfigStore
	failID string
	err    error
	armed  bool
}

func (s *rereadFailStore) Get(ctx context.Context, id string) (webhooks.Config, error) {
	if s.armed && id == s.failID {
		return webhooks.Config{}, s.err
	}
	return s.ConfigStore.Get(ctx, id)
}

// TestReprovision_FailedConfigRereadIsAConfigRestoreError: the restore is a
// compare-and-set, so a re-read it cannot perform leaves it unable to tell a
// concurrent winner from its own write. It reports that — a blind write would
// be the clobber the compare exists to prevent.
func TestReprovision_FailedConfigRereadIsAConfigRestoreError(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	res := provisionBots(t, o, "review-pr")

	errStoreDown := errors.New("webhook store unavailable")
	store := &rereadFailStore{ConfigStore: o.Webhooks, failID: res.WebhookID, err: errStoreDown}
	o.Webhooks = store
	useAdmin(o, &failingHookAdmin{
		fakeAdmin: fa,
		err:       StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity),
		before:    func() { store.armed = true },
	})

	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	var restoreFailed *ConfigRestoreError
	if !errors.As(err, &restoreFailed) {
		t.Fatalf("want a *ConfigRestoreError, got %T: %v", err, err)
	}
	if restoreFailed.WebhookID != res.WebhookID {
		t.Errorf("ConfigRestoreError.WebhookID = %q, want %q", restoreFailed.WebhookID, res.WebhookID)
	}
	if !errors.Is(err, errStoreDown) {
		t.Errorf("the store's own failure is absent from %v", err)
	}
	var refused *HookRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("the forge's refusal must survive the restore failure: %v", err)
	}
}
