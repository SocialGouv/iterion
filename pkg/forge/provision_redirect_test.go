package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type redirectedHookAdmin struct {
	*fakeAdmin
	client *http.Client
	url    string
}

func (a *redirectedHookAdmin) UpdateHook(ctx context.Context, _, _ string, spec HookSpec) (HookHandle, error) {
	var out HookHandle
	_, err := DoJSON(ctx, a.client, "PUT", a.url, "test forge", nil, spec, &out)
	return out, err
}

func TestProvisionRedirectKeepsActualForgeSecretVerifiable(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-apply", true: "after-apply"}[apply], func(t *testing.T) {
			o, fa, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			first := provisionBots(t, o, "review-pr")
			followed := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/canonical" {
					followed++
					_ = json.NewEncoder(w).Encode(HookHandle{ID: first.HookID})
					return
				}
				var spec HookSpec
				if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
					t.Error(err)
				}
				if apply {
					fa.lastSecret = spec.Secret
				}
				w.Header().Set("Location", "/canonical")
				w.WriteHeader(http.StatusSeeOther)
			}))
			defer srv.Close()
			o.AdminFor = func(context.Context, Connection) (Admin, error) {
				return &redirectedHookAdmin{fa, srv.Client(), srv.URL}, nil
			}
			_, err := o.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"revi-converse"}})
			var redirect *RedirectError
			if !errors.As(err, &redirect) || redirect.Code != 303 || !strings.Contains(err.Error(), "/canonical") {
				t.Fatalf("redirected hook write must fail with status and target: %v", err)
			}
			if followed != 0 {
				t.Fatal("write followed redirect")
			}
			cfg, e := o.Webhooks.Get(context.Background(), first.WebhookID)
			if e != nil {
				t.Fatal(e)
			}
			if !webhooks.VerifyToken(fa.lastSecret, cfg.TokenHash) {
				t.Fatal("stored config rejects forge's actual secret after redirect")
			}
		})
	}
}

type ambiguousCreateAdmin struct {
	*fakeAdmin
	fail bool
}

func (a *ambiguousCreateAdmin) CreateHook(ctx context.Context, repo string, spec HookSpec) (HookHandle, error) {
	h, err := a.fakeAdmin.CreateHook(ctx, repo, spec)
	if err == nil && a.fail {
		a.fail = false
		return HookHandle{}, errors.New("response lost after applying hook")
	}
	return h, err
}
func TestFreshIndeterminateProvisionRetainsRetryIdentity(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	a := &ambiguousCreateAdmin{fa, true}
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return a, nil }
	ctx := context.Background()
	req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}}
	_, err := o.Provision(ctx, req)
	var unknown *HookWriteIndeterminateError
	if !errors.As(err, &unknown) {
		t.Fatalf("unknown create outcome not reported: %v", err)
	}
	ri, err := o.Integrations.GetProvisionByConnRepo(ctx, "t1", "conn-1", "group/api")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := o.Webhooks.Get(ctx, ri.CandidateWebhookID)
	if err != nil {
		t.Fatal("unknown remote create lost verification config", err)
	}
	if !webhooks.VerifyToken(fa.lastSecret, cfg.TokenHash) {
		t.Fatal("applied remote create cannot verify")
	}
	oldSecret := fa.lastSecret
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.WebhookID != cfg.ID || fa.lastSecret != oldSecret || fa.creates != 1 {
		t.Fatal("retry replaced its durable identity or duplicated known hook")
	}
}
