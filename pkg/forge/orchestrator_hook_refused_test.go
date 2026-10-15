package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// refusingAdmin is a forge whose hook writes answer the way a GitLab answers
// a delivery URL outside its outbound allowlist: HTTP 422, through the very
// error the real clients build (forge.StatusErr) — a kinder fake would prove
// nothing about the typing the route reads.
type refusingAdmin struct {
	*fakeAdmin
	createErr error
	updateErr error
}

func (r *refusingAdmin) CreateHook(ctx context.Context, repo string, spec HookSpec) (HookHandle, error) {
	if r.createErr != nil {
		return HookHandle{}, r.createErr
	}
	return r.fakeAdmin.CreateHook(ctx, repo, spec)
}

func (r *refusingAdmin) UpdateHook(ctx context.Context, repo, hookID string, spec HookSpec) (HookHandle, error) {
	if r.updateErr != nil {
		return HookHandle{}, r.updateErr
	}
	return r.fakeAdmin.UpdateHook(ctx, repo, hookID, spec)
}

func useAdmin(o *Orchestrator, a Admin) {
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return a, nil }
}

// TestHookBaseURL covers the one derivation every hook address and the enable
// preview share: the pin wins, the public URL is the default, and a trailing
// slash never reaches the URL (a hook at "https://h//api/webhooks/…" is a
// path the server does not route).
func TestHookBaseURL(t *testing.T) {
	o, _, _ := newTestOrch(t)
	o.PublicURL = "https://iterion.example.com/"

	conn := Connection{Provider: ProviderGitLab}
	if got := o.HookBaseURL(conn); got != "https://iterion.example.com" {
		t.Errorf("unpinned base = %q, want the deployment's public URL without its trailing slash", got)
	}

	conn.WebhookBaseURL = "https://iterion.allowlisted.example/"
	if got := o.HookBaseURL(conn); got != "https://iterion.allowlisted.example" {
		t.Errorf("pinned base = %q, want the connection's pin without its trailing slash", got)
	}
	if got := o.inboundURL(conn, "wh-1"); got != "https://iterion.allowlisted.example/api/webhooks/gitlab/wh-1" {
		t.Errorf("inboundURL = %q — it must be built from HookBaseURL, so the preview cannot announce another host", got)
	}
}

// TestProvision_HookCreateRefusedIsTyped: a fresh provision whose hook the
// forge refuses by ADDRESS comes back typed — the route cannot name the
// remedy (pin a base) from an opaque "HTTP 422" — and still rolls its created
// config back, since an inbound endpoint no hook calls is an orphan.
func TestProvision_HookCreateRefusedIsTyped(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	useAdmin(o, &refusingAdmin{fakeAdmin: fa, createErr: StatusErr("gitlab", "create hook", http.StatusUnprocessableEntity)})

	_, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	var refused *HookRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a 422 on the hook CREATE must be a *HookRefusedError, got %T: %v", err, err)
	}
	if !strings.HasPrefix(refused.URL, "https://iterion.example.com/api/webhooks/gitlab/") {
		t.Errorf("refused URL = %q, want the address the forge was given", refused.URL)
	}
	if refused.Pinned != "" {
		t.Errorf("Pinned = %q on an unpinned connection — the remedy would send the operator to a pin that does not exist", refused.Pinned)
	}
	if refused.Provider != ProviderGitLab {
		t.Errorf("Provider = %q, want the connection's", refused.Provider)
	}
	// The forge's own answer stays reachable: every caller that classified on
	// the status keeps doing so.
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnprocessableEntity {
		t.Errorf("the forge's 422 must stay reachable through Unwrap, got %v", err)
	}

	cfgs, lerr := o.Webhooks.ListByTenant(ctx, "t1")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(cfgs) != 0 {
		t.Errorf("the refused fresh provision left %d webhook config(s) behind: %+v", len(cfgs), cfgs)
	}
}

// TestProvision_PinnedHookRefusalCarriesThePin: the pinned case has its own
// remedy (check the pinned host on the forge, or change the pin), so the pin
// must ride the error — the URL alone does not say whether it came from the
// connection or from the deployment.
func TestProvision_PinnedHookRefusalCarriesThePin(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer)
	ctx := context.Background()
	conn.WebhookBaseURL = "https://iterion.pinned.example/"
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	useAdmin(o, &refusingAdmin{fakeAdmin: fa, createErr: StatusErr("gitlab", "create hook", http.StatusUnprocessableEntity)})

	_, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	var refused *HookRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want *HookRefusedError, got %T: %v", err, err)
	}
	if refused.Pinned != "https://iterion.pinned.example" {
		t.Errorf("Pinned = %q, want the connection's pin (no trailing slash)", refused.Pinned)
	}
}

// TestProvision_NonRefusalErrorsPassThrough: only the 422 is a refused
// ADDRESS. A 403 is a grant and a 503 is an outage; dressing either as a
// refused URL sends the operator to the wrong setting.
func TestProvision_NonRefusalErrorsPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", StatusErr("gitlab", "create hook", http.StatusForbidden)},
		{"upstream down", StatusErr("gitlab", "create hook", http.StatusServiceUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, fa, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			useAdmin(o, &refusingAdmin{fakeAdmin: fa, createErr: tc.err})

			_, err := o.Provision(context.Background(), ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"review-pr"}, ActorID: "u1",
			})
			var refused *HookRefusedError
			if errors.As(err, &refused) {
				t.Fatalf("%v must not be typed as a refused hook URL", err)
			}
		})
	}
}

// TestReprovision_RefusedHookRestoresTheStoredConfig is the assertion the
// restore exists for. A re-provision mints a new iwh_ and REPLACES the stored
// config before touching the forge; when the forge then refuses the new
// address, the hook it is still delivering to carries the OLD secret. Leaving
// the new config in place turns a working repo into one whose every delivery
// fails its signature check — and the provision call "only" returned an
// error, so nothing else reports it.
func TestReprovision_RefusedHookRestoresTheStoredConfig(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	liveHookURL := hookURLFor(t, fa, "group/api")

	// The deployment moves to a host this forge is not allowed to call, and a
	// second bot triggers the full re-provision path.
	o.PublicURL = "https://iterion.cloud"
	useAdmin(o, &refusingAdmin{fakeAdmin: fa, updateErr: StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity)})
	_, err = o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	var refused *HookRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a 422 on the hook UPDATE must be a *HookRefusedError, got %T: %v", err, err)
	}

	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("the refused re-provision left no webhook config at all: %v", err)
	}
	if after.TokenHash != before.TokenHash {
		t.Errorf("the stored webhook token hash changed on a refused re-provision: the forge still delivers to %s with the previous secret, so every delivery would now fail its signature check", liveHookURL)
	}
	if !sameSet(after.BotIDs, before.BotIDs) || !sameSet(after.EventAllowlist, before.EventAllowlist) {
		t.Errorf("stored config not restored: bots %v→%v, events %v→%v", before.BotIDs, after.BotIDs, before.EventAllowlist, after.EventAllowlist)
	}
	if after.RotatedAt != nil {
		t.Errorf("the restored config carries the refused provision's rotation stamp")
	}
	// The forge-side hook never moved, so the repo keeps receiving.
	if got := hookURLFor(t, fa, "group/api"); got != liveHookURL {
		t.Errorf("hook URL = %q, want the live one %q untouched", got, liveHookURL)
	}
}

// restoreFailStore fails exactly the write that puts a given token hash back
// — the restore — while every other config write succeeds.
type restoreFailStore struct {
	webhooks.ConfigStore
	failHash string
	err      error
}

func (s *restoreFailStore) Update(ctx context.Context, c webhooks.Config) error {
	if c.TokenHash == s.failHash {
		return s.err
	}
	return s.ConfigStore.Update(ctx, c)
}

// TestReprovision_FailedRestoreReportsBothErrors: when the restore itself
// fails, the repo is left with a config the live hook's secret no longer
// matches. That is a second, worse failure than the refusal — returning only
// the refusal would send the operator to fix a pin while deliveries silently
// die.
func TestReprovision_FailedRestoreReportsBothErrors(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}

	errStoreDown := errors.New("webhook store unavailable")
	o.Webhooks = &restoreFailStore{ConfigStore: o.Webhooks, failHash: before.TokenHash, err: errStoreDown}
	useAdmin(o, &refusingAdmin{fakeAdmin: fa, updateErr: StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity)})

	_, err = o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	var refused *HookRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("the forge's refusal must survive the restore failure, got %T: %v", err, err)
	}
	if !errors.Is(err, errStoreDown) {
		t.Errorf("the failed restore is absent from the returned error (%v): the config the live hook's secret no longer matches is nobody's news", err)
	}
	var restoreFailed *ConfigRestoreError
	if !errors.As(err, &restoreFailed) {
		t.Fatalf("the failed restore must be typed, so the route reports it ahead of the refusal, got %T: %v", err, err)
	}
	if restoreFailed.WebhookID != res.WebhookID {
		t.Errorf("ConfigRestoreError.WebhookID = %q, want the repo's webhook %q", restoreFailed.WebhookID, res.WebhookID)
	}
}
