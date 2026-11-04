package server

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The webhook CRUD addresses a config by an id the CALLER supplies, and the
// team gate above it only proves standing in the team named in the PATH. So
// every by-id handler re-reads the config and asserts it belongs to that
// team (webhookForTenant's `cfg.TenantID != teamID`): without the clause, an
// admin of team A would read, patch, delete and ROTATE team B's webhook by
// guessing its id — a rotate alone bricks the victim's deliveries. Five
// handlers depend on that one clause, and nothing drove any of them with a
// foreign config.

// foreignWebhookIDs are the two configs every row below works with: one
// owned by the team in the request path, one stamped with the other team.
const (
	ownWebhookID     = "wh-own-a"
	foreignWebhookID = "wh-foreign-b"
)

// webhookForeignFixture is newRunScopeServer (tenant-A and tenant-B are real
// teams of org-1, u-both is an ADMIN of tenant-A — so canManageTeam passes
// and the refusal under test is the only thing left to answer) with the
// webhook CRUD mounted through its own production registrar and a config
// store holding one config per team.
//
// The registrar is called here rather than by New(): a Config with no
// WebhookConfigs skips it (server_routes.go), and a fixture that skipped it
// would drive nothing at all.
//
// Each row builds its own fixture: the delete and the rotate MUTATE the
// store, so a shared one would make a row's verdict depend on the order the
// rows ran in.
func webhookForeignFixture(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv, _ := newRunScopeServer(t)
	srv.webhookConfigs = webhooks.NewMemoryConfigStore()
	srv.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	srv.registerWebhookRoutes()
	ctx := context.Background()
	for _, cfg := range []webhooks.Config{
		{ID: ownWebhookID, TenantID: "tenant-A", Name: "own", Provider: webhooks.ProviderGitLab, Enabled: true, BotIDs: []string{"review-pr"}},
		{ID: foreignWebhookID, TenantID: "tenant-B", Name: "foreign", Provider: webhooks.ProviderGitLab, Enabled: true, BotIDs: []string{"review-pr"}},
	} {
		if err := srv.webhookConfigs.Create(ctx, cfg); err != nil {
			t.Fatalf("seed webhook %s: %v", cfg.ID, err)
		}
	}
	return srv, fullStack(t, srv)
}

// webhookForeignCases is one row per webhookForTenant caller, each named by
// the handler it drives: every by-id route of the family, asked under
// tenant-A's path for a config stamped tenant-B.
var webhookForeignCases = []struct {
	handler string // the caller, as declared in webhooks_routes.go
	method  string
	path    string // %s is the webhook id
	body    string
	served  int // what the SAME request answers for the team's own config
}{
	{"handleGetWebhook", http.MethodGet, "/api/teams/tenant-A/webhooks/%s", "", http.StatusOK},
	{"handleUpdateWebhook", http.MethodPatch, "/api/teams/tenant-A/webhooks/%s", `{}`, http.StatusOK},
	{"handleDeleteWebhook", http.MethodDelete, "/api/teams/tenant-A/webhooks/%s", "", http.StatusNoContent},
	{"handleRotateWebhook", http.MethodPost, "/api/teams/tenant-A/webhooks/%s/rotate", "", http.StatusOK},
	{"handleListWebhookDeliveries", http.MethodGet, "/api/teams/tenant-A/webhooks/%s/deliveries", "", http.StatusOK},
}

// Every caller of the chokepoint, driven with a FOREIGN tenant's config: the
// 404 an absent webhook answers, from an admin of the team in the path. The
// second leg of each row is what keeps it from being vacuous — the SAME
// request for the team's OWN config is served — so a row cannot pass because
// the route 404s for a reason of its own (a pattern that stopped matching, a
// gate above refusing first).
func TestEveryWebhookByIDCallerRefusesAForeignConfig(t *testing.T) {
	admin := caller("u-both", "tenant-A")
	for _, tc := range webhookForeignCases {
		t.Run(tc.handler, func(t *testing.T) {
			srv, ts := webhookForeignFixture(t)
			code, body := boardReq(t, srv, ts, tc.method, fmt.Sprintf(tc.path, foreignWebhookID), tc.body, admin)
			if code != http.StatusNotFound {
				t.Fatalf("%s on tenant-B's webhook under tenant-A's path = %d %q, want 404 — the config belongs to another team", tc.handler, code, body)
			}
			// The foreign config must also be INTACT: a rotate or a delete
			// that answered 404 after touching the victim's row would pass
			// the assertion above.
			foreign, err := srv.webhookConfigs.Get(context.Background(), foreignWebhookID)
			if err != nil {
				t.Fatalf("%s left tenant-B's webhook unreadable: %v", tc.handler, err)
			}
			if foreign.TenantID != "tenant-B" || foreign.Name != "foreign" || foreign.Version != 0 || foreign.RotatedAt != nil {
				t.Fatalf("%s changed tenant-B's webhook: %+v", tc.handler, foreign)
			}

			srv, ts = webhookForeignFixture(t)
			if code, body := boardReq(t, srv, ts, tc.method, fmt.Sprintf(tc.path, ownWebhookID), tc.body, admin); code != tc.served {
				t.Fatalf("%s on the team's OWN webhook = %d %q, want %d — this row's 404 would prove nothing about foreignness", tc.handler, code, body, tc.served)
			}
		})
	}
}

// The registrar's own class, read from its source: a by-id handler added to
// webhooks_routes.go that does NOT re-read its config through the chokepoint
// — or does, but is absent from the table above — reddens here. The five
// rows cannot cover for each other, and a new one cannot slip in untested.
// The class of HTTP sites that address a webhook by a caller-supplied id is
// WIDER than this file and than the `webhook_id` path value; the witness
// below reads that one, keyed on the gesture.
func TestEveryWebhookByIDHandlerGoesThroughTheChokepoint(t *testing.T) {
	const src = "webhooks_routes.go"
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	code := lineComments.ReplaceAllString(string(raw), "")
	driven := map[string]bool{}
	for _, tc := range webhookForeignCases {
		driven[tc.handler] = true
	}
	handlerHead := regexp.MustCompile(`func \(s \*Server\) (handle\w+)\(w http\.ResponseWriter, r \*http\.Request\) \{`)
	heads := handlerHead.FindAllStringSubmatchIndex(code, -1)
	// Seven handlers of this shape live in the file today (the five by-id
	// ones plus the list and the create); under six, this witness is
	// reading something other than the family it guards.
	if len(heads) < 6 {
		t.Fatalf("parsed %d handler(s) from %s, floor 6 — this witness no longer reads the file it guards", len(heads), src)
	}
	byID := 0
	for i, h := range heads {
		name := code[h[2]:h[3]]
		end := len(code)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		body := code[h[1]:end]
		if !strings.Contains(body, `PathValue("webhook_id")`) {
			continue
		}
		byID++
		if !strings.Contains(body, "s.webhookForTenant(") {
			t.Errorf("%s addresses a webhook by id and never re-reads it through webhookForTenant — the team gate above it only proves standing in the team named in the PATH, so another team's config is reachable by id", name)
		}
		if !driven[name] {
			t.Errorf("%s addresses a webhook by id and no row of webhookForeignCases drives it with a foreign config — add its row", name)
		}
	}
	if byID != len(webhookForeignCases) {
		t.Fatalf("%s holds %d by-id handler(s) and the table drives %d — the table and the file disagree about the class", src, byID, len(webhookForeignCases))
	}
}

// The clause itself, at the chokepoint, so its two terms are both witnessed:
// an id that names nothing and an id that names another team's config answer
// the SAME 404 — a caller cannot tell a foreign webhook from an absent one,
// which is what keeps ids from being enumerable across tenants.
func TestWebhookForTenantAnswersOneRefusalForAbsentAndForeign(t *testing.T) {
	srv, ts := webhookForeignFixture(t)
	admin := caller("u-both", "tenant-A")
	absent, _ := boardReq(t, srv, ts, http.MethodGet, "/api/teams/tenant-A/webhooks/wh-nope", "", admin)
	foreign, _ := boardReq(t, srv, ts, http.MethodGet, "/api/teams/tenant-A/webhooks/"+foreignWebhookID, "", admin)
	if absent != http.StatusNotFound || foreign != absent {
		t.Fatalf("absent id = %d, foreign config = %d: both are the same 404", absent, foreign)
	}
}

// A member of the OTHER team is refused by the team gate first, not by the
// chokepoint: the two refusals are distinct, and this is what proves the
// rows above really exercise the foreignness clause rather than the gate.
func TestAForeignCallerIsRefusedByTheTeamGateNotTheChokepoint(t *testing.T) {
	srv, ts := webhookForeignFixture(t)
	outsider := caller("u-member-b", "tenant-B")
	code, body := boardReq(t, srv, ts, http.MethodGet, "/api/teams/tenant-A/webhooks/"+ownWebhookID, "", outsider)
	if code != http.StatusForbidden {
		t.Fatalf("a member of tenant-B reading tenant-A's webhook = %d %q, want 403 from the team gate", code, body)
	}
}

// The SIXTH caller, on the lane the table above cannot drive: the credential
// preview takes its webhook id from the request BODY, so no `webhook_id`
// path value names it and no row of webhookForeignCases reaches it. It shares
// the CRUD's clause (readWebhookForTenant), and here is that clause on this
// lane: another team's config is the same 404 an absent id answers, and the
// team's own is served — so the 404 is foreignness and not the route.
func TestTheCredentialPreviewRefusesAForeignWebhook(t *testing.T) {
	srv, rs := newTeamForkServer(t, &tierPublisher{})
	srv.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(&credentialPreviewPublisher{}))
	configs := webhooks.NewMemoryConfigStore()
	srv.webhookConfigs = configs
	for _, cfg := range []webhooks.Config{
		{ID: "hook-own", TenantID: "t1", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe"},
		{ID: "hook-foreign", TenantID: "t2", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe"},
	} {
		if err := configs.Create(t.Context(), cfg); err != nil {
			t.Fatalf("seed webhook %s: %v", cfg.ID, err)
		}
	}
	id := auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember}
	absent := callCredentialPreview(srv, `{"source":{"kind":"webhook","id":"hook-nope"}}`, id)
	foreign := callCredentialPreview(srv, `{"source":{"kind":"webhook","id":"hook-foreign"}}`, id)
	if foreign.Code != http.StatusNotFound || foreign.Code != absent.Code {
		t.Fatalf("preview of t2's webhook = %d %q, absent id = %d — both are the same 404, or ids are enumerable across tenants", foreign.Code, foreign.Body.String(), absent.Code)
	}
	if own := callCredentialPreview(srv, `{"source":{"kind":"webhook","id":"hook-own"}}`, id); own.Code != http.StatusOK {
		t.Fatalf("preview of the team's OWN webhook = %d %q, want 200 — the 404 above would prove nothing about foreignness", own.Code, own.Body.String())
	}
}

// webhookConfigReaders is the complete census of the functions that read a
// webhook config out of the store, each with the id's PROVENANCE — the one
// fact that decides whether the foreignness clause is owed. Exactly one
// reader takes an id from a CALLER; every other lane takes it from a stored
// record and cross-checks it against that record's own tenant.
var webhookConfigReaders = map[string]string{
	"readWebhookForTenant":      "THE chokepoint: the id comes from the caller — a path value for the CRUD, the request body for the credential preview — so it carries the `cfg.TenantID != teamID` clause and answers one 404 for absent and for foreign",
	"webhookAuth":               "inbound delivery: the id is the delivery URL's own and the config's token or HMAC authenticates the request, so no team standing is claimed",
	"prepareWebhookRetry":       "retry sweeper: the id comes from the stored delivery, re-checked against that delivery's own tenant",
	"fireDeferredWebhookLaunch": "debounce timer: the id comes from the stored deferred launch",
	"cardLaunchPolicyLayers":    "dispatcher: the id comes from the board card's launch context, under the tenant the card carries",
	"armedReviewRequestLogins":  "forge publish: the id comes from the publish grant, re-checked against the grant's team",
	"repoLaunchPolicy":          "forge publish: the id comes from the repo integration, re-checked against the integration's tenant",
	"autofixForRunID":           "forge auto-fix lane: the id comes from the run's repo integration",
	"relaunchDeadGateRun":       "dead-gate relaunch: the id comes from the run's repo integration",
}

// The WHOLE class, keyed on the GESTURE rather than on one file and one id
// shape: every function that reads a webhook config out of the store, named
// with where its id came from. handleCredentialPreview is the site that
// proves the point — it took the id from the request BODY, lived in another
// file, and re-implemented the clause inline (`err == nil && cfg.TenantID !=
// teamID`), so the file-and-PathValue witness above said nothing about it
// while webhookForTenant was not a chokepoint at all.
//
// A reader absent from the census reddens here: it is either a new
// caller-facing surface that owes the clause, or a new machine lane that owes
// its reason. The function bodies are read from the AST, not sliced between
// regexp matches — a helper declared after a handler is NOT part of it.
func TestEveryWebhookConfigReaderIsNamedWithItsIDProvenance(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the server package: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	sites := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fd.Body == nil {
				continue
			}
			reads := 0
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if get, ok := call.Fun.(*ast.SelectorExpr); ok && get.Sel.Name == "Get" {
					if store, ok := get.X.(*ast.SelectorExpr); ok && store.Sel.Name == "webhookConfigs" {
						reads++
					}
				}
				return true
			})
			if reads == 0 {
				continue
			}
			sites += reads
			seen[fd.Name.Name] = true
			if _, named := webhookConfigReaders[fd.Name.Name]; named {
				continue
			}
			if answersACaller(fd) {
				t.Errorf("%s (%s) answers a caller AND reads a webhook config from the store itself — address one by a caller-supplied id only through readWebhookForTenant, which is where the `cfg.TenantID != teamID` clause lives; a second copy of that clause is a tenant leak one edit away", fd.Name.Name, name)
				continue
			}
			t.Errorf("%s (%s) reads a webhook config and is absent from webhookConfigReaders — say where its id comes from, or take it through readWebhookForTenant if it comes from a caller", fd.Name.Name, name)
		}
	}
	for reader := range webhookConfigReaders {
		if !seen[reader] {
			t.Errorf("webhookConfigReaders names %s and no reader in the package is called that — a renamed or deleted lane leaves a row that guards nothing", reader)
		}
	}
	// Non-vacuity: nine readers hold the gesture today. A scan that found
	// fewer is reading something other than the class it guards.
	if sites < 9 {
		t.Fatalf("found %d webhookConfigs.Get site(s), floor 9 — the scan no longer reads the class", sites)
	}
}

// answersACaller reports whether fd can write an HTTP response itself: a
// method taking an http.ResponseWriter. Such a function is reached by a
// request, so an id it addresses a webhook by may be the caller's.
func answersACaller(fd *ast.FuncDecl) bool {
	if fd.Type.Params == nil {
		return false
	}
	for _, param := range fd.Type.Params.List {
		if sel, ok := param.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ResponseWriter" {
			return true
		}
	}
	return false
}
