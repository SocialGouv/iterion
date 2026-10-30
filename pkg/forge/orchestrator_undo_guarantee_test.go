package forge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// ---- the forge side of the matrix ----

// forgeHook is one webhook as the FORGE holds it: the address it delivers to
// and the secret it signs those deliveries with.
type forgeHook struct {
	id     string
	url    string
	secret string
}

// matrixForge keeps EVERY hook it is given, each with the secret it was last
// written with: GitLab and Forgejo both accept two hooks on one delivery URL,
// so a fake holding one hook per repo would hide a duplicate. It fails one
// chosen step of the hook upsert with one chosen error — applying the spec
// first when the answer is the thing that was lost — and runs beforeFail at
// that step, which is where the matrix kills the request's context.
type matrixForge struct {
	hooks  []forgeHook
	nextID int

	// failStep is "get", "update" or "create"; empty fails nothing.
	failStep        string
	failErr         error
	applyBeforeFail bool
	beforeFail      func()
	// afterApply runs once the forge has committed a hook write it answers
	// successfully — where a request dies between the forge accepting and
	// iterion recording it.
	afterApply func()

	gets, updates, creates int
}

func (f *matrixForge) Provider() Provider { return ProviderGitLab }

func (f *matrixForge) WhoAmI(context.Context) (Identity, error) {
	return Identity{Login: "bot", Kind: "user"}, nil
}

func (f *matrixForge) ListRepos(context.Context, RepoQuery) ([]RepoSummary, error) {
	return nil, nil
}

func (f *matrixForge) GetHook(_ context.Context, _, deliveryURL string) (*HookHandle, error) {
	f.gets++
	if f.failStep == "get" {
		f.fire()
		return nil, f.failErr
	}
	for _, h := range f.hooks {
		if h.url == deliveryURL {
			return &HookHandle{ID: h.id, URL: h.url, Active: true}, nil
		}
	}
	return nil, nil
}

func (f *matrixForge) CreateHook(_ context.Context, _ string, spec HookSpec) (HookHandle, error) {
	f.creates++
	if f.failStep == "create" {
		if f.applyBeforeFail {
			f.apply("", spec)
		}
		f.fire()
		return HookHandle{}, f.failErr
	}
	h := f.apply("", spec)
	if f.afterApply != nil {
		f.afterApply()
	}
	return h, nil
}

func (f *matrixForge) UpdateHook(_ context.Context, _, hookID string, spec HookSpec) (HookHandle, error) {
	f.updates++
	// A hook id the forge does not know answers like the real ones' 404:
	// ErrHookNotFound, which is what a stale stored id produces.
	if f.indexOf(hookID) < 0 {
		return HookHandle{}, ErrHookNotFound
	}
	if f.failStep == "update" {
		if f.applyBeforeFail {
			f.apply(hookID, spec)
		}
		f.fire()
		return HookHandle{}, f.failErr
	}
	return f.apply(hookID, spec), nil
}

func (f *matrixForge) DeleteHook(_ context.Context, _, hookID string) error {
	i := f.indexOf(hookID)
	if i < 0 {
		return ErrHookNotFound
	}
	f.hooks = append(f.hooks[:i], f.hooks[i+1:]...)
	return nil
}

func (f *matrixForge) ListHooks(context.Context, string) ([]HookHandle, error) {
	out := make([]HookHandle, 0, len(f.hooks))
	for _, h := range f.hooks {
		out = append(out, HookHandle{ID: h.id, URL: h.url, Active: true})
	}
	return out, nil
}

func (f *matrixForge) fire() {
	if f.beforeFail != nil {
		f.beforeFail()
	}
}

func (f *matrixForge) indexOf(hookID string) int {
	for i, h := range f.hooks {
		if h.id == hookID {
			return i
		}
	}
	return -1
}

// apply records what the forge now holds: a fresh hook for an empty id, the
// named one rewritten in place otherwise.
func (f *matrixForge) apply(hookID string, spec HookSpec) HookHandle {
	if i := f.indexOf(hookID); i >= 0 {
		f.hooks[i].url = spec.URL
		f.hooks[i].secret = spec.Secret
		return HookHandle{ID: hookID, URL: spec.URL, Events: spec.Events, Active: spec.Active}
	}
	f.nextID++
	h := forgeHook{id: fmt.Sprintf("hook-%d", f.nextID), url: spec.URL, secret: spec.Secret}
	f.hooks = append(f.hooks, h)
	return HookHandle{ID: h.id, URL: h.url, Events: spec.Events, Active: spec.Active}
}

// only answers the single hook the forge holds, failing the test otherwise:
// every row's premise is one provisioned hook.
func (f *matrixForge) only(t *testing.T) forgeHook {
	t.Helper()
	if len(f.hooks) != 1 {
		t.Fatalf("the forge holds %d hooks, want exactly 1: %+v", len(f.hooks), f.hooks)
	}
	return f.hooks[0]
}

// duplicateURL answers a delivery URL the forge holds TWO hooks on, which is
// every event delivered twice and every bot run doubled.
func (f *matrixForge) duplicateURL() (string, bool) {
	seen := map[string]int{}
	for _, h := range f.hooks {
		seen[h.url]++
		if seen[h.url] > 1 {
			return h.url, true
		}
	}
	return "", false
}

// ctxStrictConfigStore models the PRODUCTION webhook config store, which
// hands the request's context to the mongo driver (pkg/webhooks/mongo.go): a
// read or a write on a context that is already done fails. The in-memory
// store ignores the context, so on it a repair that the request's
// cancellation defeats still appears to run.
type ctxStrictConfigStore struct {
	webhooks.ConfigStore
}

func (s ctxStrictConfigStore) Create(ctx context.Context, c webhooks.Config) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: create config %s: %w", c.ID, err)
	}
	return s.ConfigStore.Create(ctx, c)
}

func (s ctxStrictConfigStore) Get(ctx context.Context, id string) (webhooks.Config, error) {
	if err := ctx.Err(); err != nil {
		return webhooks.Config{}, fmt.Errorf("store: get config %s: %w", id, err)
	}
	return s.ConfigStore.Get(ctx, id)
}

func (s ctxStrictConfigStore) Update(ctx context.Context, c webhooks.Config) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: update config %s: %w", c.ID, err)
	}
	return s.ConfigStore.Update(ctx, c)
}

func (s ctxStrictConfigStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: delete config %s: %w", id, err)
	}
	return s.ConfigStore.Delete(ctx, id)
}

// ---- the matrix ----

// undoFailureKind is one way a step can fail. determinate says the failure
// PROVES the forge did not apply the write, which is the whole condition for
// putting the previous config back.
type undoFailureKind struct {
	name string
	// err builds the failure for the operation the point names ("update
	// hook", "create hook", "get hooks"), the way the provider clients do.
	err func(op string) error
	// applied: the forge committed the write and then failed to answer, so
	// its hook signs with the NEW secret.
	applied bool
	// cancelsRequest: the request's context dies at the failing step — the
	// operator's tab closed, a gateway read timeout on a slow forge.
	cancelsRequest bool
	determinate    bool
}

// transportErr wraps a transport failure the way net/http does: every error
// a *http.Client answers with is a *url.Error around the cause.
func transportErr(cause error) error {
	return &url.Error{Op: "Put", URL: "https://gitlab.example.com/api/v4/projects/group%2Fapi/hooks/hook-1", Err: cause}
}

func undoFailureKinds() []undoFailureKind {
	status := func(code int) func(string) error {
		return func(op string) error { return StatusErr("gitlab", op, code) }
	}
	transport := func(cause error) func(string) error {
		return func(string) error { return transportErr(cause) }
	}
	return []undoFailureKind{
		{name: "403 the token lost hook admin", err: status(http.StatusForbidden), determinate: true},
		{name: "404 the hook is gone", err: status(http.StatusNotFound), determinate: true},
		{name: "422 the address is refused", err: status(http.StatusUnprocessableEntity), determinate: true},
		// A gateway answers 408/499 for a request it may already have
		// forwarded, so neither proves a refusal.
		{name: "408 gateway request timeout", err: status(http.StatusRequestTimeout)},
		{name: "499 client closed request", err: status(statusClientClosedRequest)},
		{name: "502 the forge fell over", err: status(http.StatusBadGateway)},
		// The answer never came: the same transport error as the applied row
		// below, in the world where the call did not land. Nothing on the
		// client's side tells the two apart, so a WRITE stays indeterminate —
		// while the same timeout on a READ proves the hook is untouched.
		{name: "timeout with no answer", err: transport(errors.New(
			"context deadline exceeded (Client.Timeout exceeded while awaiting headers)"))},
		{name: "dial refused", err: transport(&net.OpError{Op: "dial", Net: "tcp",
			Err: errors.New("connect: connection refused")}), determinate: true},
		{name: "dns no such host", err: transport(&net.DNSError{
			Err: "no such host", Name: "gitlab.internal.example", IsNotFound: true}), determinate: true},
		{name: "tls certificate expired", err: transport(&tls.CertificateVerificationError{
			Err: x509.CertificateInvalidError{Reason: x509.Expired}}), determinate: true},
		{name: "x509 unknown authority", err: transport(x509.UnknownAuthorityError{}), determinate: true},
		// The write LANDED and the answer was lost: the forge holds the new
		// secret, so the stored config this attempt wrote is the one matching
		// it and restoring is what would break the repo.
		{name: "write timeout after the forge applied", applied: true,
			err: transport(errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"))},
		// The determinate refusal this feature exists for, answered to a
		// request whose client is already gone: the repair is owed and the
		// context it was asked on cannot carry it.
		{name: "422 with the request cancelled mid-call", cancelsRequest: true,
			err: status(http.StatusUnprocessableEntity), determinate: true},
	}
}

// undoFailurePoint is one step between the config write and a forge-side hook
// that matches it.
type undoFailurePoint struct {
	name string
	// beforeForge marks the one point reached before any forge call, where
	// every kind is determinate whatever it is.
	beforeForge bool
	// wroteNothing marks a point whose forge call is a READ: it registers
	// nothing, so — exactly like beforeForge — the forge's hook is the one it
	// had whatever the failure, and the previous config is owed back. A
	// classification by status alone calls the 5xx rows here indeterminate and
	// leaves a working repo on a secret the forge never received.
	wroteNothing bool
	// skips names the kinds this point cannot express.
	skips map[string]string
	// arm makes the re-provision fail here with k.
	arm func(t *testing.T, o *Orchestrator, f *matrixForge, k undoFailureKind, cancelMid func())
}

func undoFailurePoints() []undoFailurePoint {
	nothingApplied := map[string]string{
		"write timeout after the forge applied": "this step writes nothing, so there is nothing to apply",
	}
	return []undoFailurePoint{{
		name:        "admin client build",
		beforeForge: true,
		skips:       nothingApplied,
		arm: func(_ *testing.T, o *Orchestrator, _ *matrixForge, k undoFailureKind, cancelMid func()) {
			o.AdminFor = func(context.Context, Connection) (Admin, error) {
				cancelMid()
				return nil, fmt.Errorf("gitlab: installation token: %w", k.err("GET token"))
			}
		},
	}, {
		name: "hook update",
		arm: func(_ *testing.T, _ *Orchestrator, f *matrixForge, k undoFailureKind, cancelMid func()) {
			f.failStep, f.failErr = "update", k.err("update hook")
			f.applyBeforeFail, f.beforeFail = k.applied, cancelMid
		},
	}, {
		name: "hook probe by URL",
		// The update of the stored (now stale) hook id answers 404 first, so
		// every row here is a probe failure AFTER a settled write.
		wroteNothing: true,
		skips:        nothingApplied,
		arm: func(t *testing.T, o *Orchestrator, f *matrixForge, k undoFailureKind, cancelMid func()) {
			staleHookID(t, o)
			f.failStep, f.failErr = "get", k.err("get hooks")
			f.beforeFail = cancelMid
		},
	}, {
		name: "hook create after a gone hook",
		arm: func(t *testing.T, o *Orchestrator, f *matrixForge, k undoFailureKind, cancelMid func()) {
			// The stored id resolves to nothing and the pin moves the
			// delivery URL, so the probe finds no hook there either: the
			// upsert reaches CreateHook, while the forge's previous hook
			// keeps delivering from the address it was registered at.
			staleHookID(t, o)
			pinWebhookBase(t, o, "https://iterion.allowlisted.example")
			f.failStep, f.failErr = "create", k.err("create hook")
			f.applyBeforeFail, f.beforeFail = k.applied, cancelMid
		},
	}}
}

// staleHookID points the stored integration at a hook id the forge does not
// know — a project import that renumbered the hooks, a hand-deleted one.
func staleHookID(t *testing.T, o *Orchestrator) {
	t.Helper()
	ri, err := o.Integrations.GetByConnRepo(context.Background(), "t1", "conn-1", "group/api")
	if err != nil {
		t.Fatal(err)
	}
	ri.HookID = "gone-77"
	if err := o.Integrations.Update(context.Background(), ri); err != nil {
		t.Fatal(err)
	}
}

// pinWebhookBase pins the connection's hook base, which is what moves the
// delivery URL of the next provision.
func pinWebhookBase(t *testing.T, o *Orchestrator, base string) {
	t.Helper()
	conn, err := o.Connections.Get(context.Background(), "conn-1")
	if err != nil {
		t.Fatal(err)
	}
	conn.WebhookBaseURL = base
	if err := o.Connections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}

// TestProvision_UndoGuaranteeMatrix holds the GUARANTEE of the undo over the
// whole surface it covers: every step between the config write and a
// forge-side hook that matches it (the POINTS) × every way that step fails
// (the KINDS), for a re-provision of a WORKING repo.
//
// Two end states are admissible, and no third: every hook the forge holds
// signs with a secret the stored config verifies (webhooks.VerifyToken — the
// predicate the inbound middleware applies to every delivery), or the error
// is typed HookWriteIndeterminateError and tells the operator to re-run. The
// third state is the defect: a stored config and a live hook on different
// secrets, every delivery answering 401, and nothing reporting it.
//
// Each row also pins WHICH of the two it must be, since a classification that
// called everything indeterminate would satisfy the disjunction while leaving
// repos broken for the length of an outage.
func TestProvision_UndoGuaranteeMatrix(t *testing.T) {
	bg := context.Background()
	for _, p := range undoFailurePoints() {
		for _, k := range undoFailureKinds() {
			if why, skip := p.skips[k.name]; skip {
				t.Run(p.name+"/"+k.name, func(t *testing.T) { t.Skipf("[fixture] matrix combination is inapplicable: %s", why) })
				continue
			}
			t.Run(p.name+"/"+k.name, func(t *testing.T) {
				o, _, sealer := newTestOrch(t)
				seedConn(t, o, sealer)
				o.Webhooks = ctxStrictConfigStore{ConfigStore: o.Webhooks}
				f := &matrixForge{}
				o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }

				// The premise: a working repo. The forge holds the hook this
				// provision registered, signing with the secret the stored
				// config verifies.
				res := provisionBots(t, o, "review-pr")
				prev := f.only(t)
				prevCfg, err := o.Webhooks.Get(bg, res.WebhookID)
				if err != nil {
					t.Fatal(err)
				}
				if !webhooks.VerifyToken(prev.secret, prevCfg.TokenHash) {
					t.Fatalf("premise: the provisioned repo does not even verify")
				}

				rctx, cancel := context.WithCancel(bg)
				defer cancel()
				cancelMid := func() {
					if k.cancelsRequest {
						cancel()
					}
				}
				p.arm(t, o, f, k, cancelMid)

				_, perr := o.Provision(rctx, ProvisionRequest{
					TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
					BotIDs: []string{"revi-converse"}, ActorID: "u1",
				})
				if perr == nil {
					t.Fatalf("the failure armed at %s did not surface", p.name)
				}

				if dup, found := f.duplicateURL(); found {
					t.Errorf("the forge holds two hooks on %s — every event is delivered twice: %+v", dup, f.hooks)
				}
				cfg, err := o.Webhooks.Get(bg, res.WebhookID)
				if err != nil {
					t.Fatalf("the failed re-provision left no stored webhook config at all: %v", err)
				}

				allVerify := true
				for _, h := range f.hooks {
					if !webhooks.VerifyToken(h.secret, cfg.TokenHash) {
						allVerify = false
					}
				}
				var indeterminate *HookWriteIndeterminateError
				saidReRun := errors.As(perr, &indeterminate) &&
					strings.Contains(perr.Error(), "re-run the same request")
				if !allVerify && !saidReRun {
					t.Errorf("BROKEN: the forge's live hook signs with a secret the stored config rejects, and the "+
						"error neither types the write as indeterminate nor says to re-run — every delivery to this "+
						"repo answers 401 and nothing reports it\n  forge hooks: %+v\n  error: %v", f.hooks, perr)
				}

				if p.beforeForge || p.wroteNothing || k.determinate {
					if !webhooks.VerifyToken(prev.secret, cfg.TokenHash) {
						t.Errorf("the failure proves the forge did not apply the write, so the previous config is "+
							"owed back and is not there\n  forge hooks: %+v\n  error: %v", f.hooks, perr)
					}
					if errors.As(perr, &indeterminate) {
						t.Errorf("a failure the forge settled must not be answered as indeterminate: %v", perr)
					}
				} else {
					if !errors.As(perr, &indeterminate) {
						t.Errorf("an unsettled write must be typed *HookWriteIndeterminateError, got %T: %v", perr, perr)
					}
					if !saidReRun {
						t.Errorf("the answer to an unsettled write does not name the remedy: %v", perr)
					}
					if cfg.TokenHash != prevCfg.TokenHash {
						t.Errorf("same-epoch retry changed its immutable secret")
					}
				}
			})
		}
	}
}

// failingIntegrationCreate fails the integration record a fresh provision
// writes AFTER the forge accepted the hook — the other step whose rollback is
// the created config.
type failingIntegrationCreate struct {
	RepoIntegrationStore
	err error
}

func (s failingIntegrationCreate) Create(context.Context, RepoIntegration) error { return s.err }

// TestFreshProvision_CancelledRequestLeavesNoOrphanConfig: a FIRST provision
// that fails after the config write has a config nothing will ever call, and
// deleting it is the undo. The request that asked for it is often already
// gone — its cancellation is what killed the call — so the delete cannot run
// on that context. Both steps that roll a created config back are covered:
// the hook write, and the integration record written after it.
func TestFreshProvision_CancelledRequestLeavesNoOrphanConfig(t *testing.T) {
	bg := context.Background()
	for _, tc := range []struct {
		name string
		arm  func(o *Orchestrator, f *matrixForge, cancel func())
	}{{
		name: "the forge refuses the hook",
		arm: func(_ *Orchestrator, f *matrixForge, cancel func()) {
			f.failStep = "create"
			f.failErr = StatusErr("gitlab", "create hook", http.StatusUnprocessableEntity)
			f.beforeFail = cancel
		},
	}, {
		name: "the integration record fails after the hook landed",
		arm: func(o *Orchestrator, f *matrixForge, cancel func()) {
			o.Integrations = failingIntegrationCreate{
				RepoIntegrationStore: o.Integrations,
				err:                  errors.New("integration store unavailable"),
			}
			f.beforeFail = cancel
			f.afterApply = cancel
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			o.Webhooks = ctxStrictConfigStore{ConfigStore: o.Webhooks}
			f := &matrixForge{}
			o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }

			rctx, cancel := context.WithCancel(bg)
			defer cancel()
			tc.arm(o, f, cancel)

			if _, err := o.Provision(rctx, ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/fresh",
				BotIDs: []string{"review-pr"}, ActorID: "u1",
			}); err == nil {
				t.Fatal("expected the failure to surface")
			}
			cfgs, err := o.Webhooks.ListByTenant(bg, "t1")
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cfgs {
				if _, err := o.Integrations.Get(bg, c.ProvisionIntegrationID); !errors.Is(err, ErrIntegrationNotFound) {
					t.Errorf("unconfirmed retry candidate is operational: %v", err)
				}
				if ri, err := o.Integrations.GetProvision(bg, c.ProvisionIntegrationID); err != nil || ri.CandidateWebhookID != c.ID {
					t.Errorf("candidate lacks a durable retry reservation: %v", err)
				}
			}
		})
	}
}

// TestUpsertHook_ProbeFailureIsNotNoHook: GetHook answers (nil, nil) for "no
// hook at this URL", so an error from it is never that. Reading it as "none"
// sends the upsert to CreateHook, which on GitLab and Forgejo registers a
// SECOND hook on the same delivery URL — and the provision returns success,
// so nothing ever reports the doubled deliveries.
func TestUpsertHook_ProbeFailureIsNotNoHook(t *testing.T) {
	bg := context.Background()
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	f := &matrixForge{}
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }
	res := provisionBots(t, o, "review-pr")
	live := f.only(t)

	staleHookID(t, o)
	f.failStep = "get"
	f.failErr = StatusErr("gitlab", "get hooks", http.StatusTooManyRequests)
	creates := f.creates

	_, err := o.Provision(bg, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	// Not fatal: the duplicate below is the half of the defect an operator
	// pays for, and a provision that answered nil is exactly how it ships.
	if err == nil {
		t.Error("a provision that could not probe the forge's hooks reported success")
	} else {
		if !strings.Contains(err.Error(), "probe hook by URL") {
			t.Errorf("the error does not name the step that failed: %v", err)
		}
		if !errors.Is(err, f.failErr) {
			t.Errorf("the forge's own failure is no longer reachable: %v", err)
		}
	}
	if f.creates != creates {
		t.Errorf("CreateHook was called %d time(s) after a failed probe: the forge now holds %+v", f.creates-creates, f.hooks)
	}
	if dup, found := f.duplicateURL(); found {
		t.Errorf("the forge holds two hooks on %s — every event delivered twice, every bot run doubled: %+v", dup, f.hooks)
	}
	if len(f.hooks) == 1 && f.hooks[0] != live {
		t.Errorf("the forge-side hook changed to %+v, want the untouched %+v", f.hooks[0], live)
	}
	cfg, gerr := o.Webhooks.Get(bg, res.WebhookID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !webhooks.VerifyToken(live.secret, cfg.TokenHash) {
		t.Errorf("a probe failure proves the forge applied nothing, so the previous config is owed back")
	}
}

// TestMintedWebhookSecretNeedsNoEscaping holds the assumption scrubHookSecret
// rests on: a minted webhook secret is URL- and JSON-safe, so the bytes as
// sent are the only spelling a forge can echo back. A token charset that
// stops being safe reddens here, where the scrub would otherwise start
// missing a form.
func TestMintedWebhookSecretNeedsNoEscaping(t *testing.T) {
	mintCtx := auth.ContextWithMintVerdict(context.Background(), auth.NewMintVerdict(auth.MintTeamToken, "test"))
	for i := 0; i < 32; i++ {
		plaintext, _, _, _, err := webhooks.MintToken(mintCtx)
		if err != nil {
			t.Fatal(err)
		}
		if got := url.QueryEscape(plaintext); got != plaintext {
			t.Fatalf("query-escaped form %q differs from the secret as sent", got)
		}
		if got := url.PathEscape(plaintext); got != plaintext {
			t.Fatalf("path-escaped form %q differs from the secret as sent", got)
		}
		if quoted := fmt.Sprintf("%q", plaintext); quoted != `"`+plaintext+`"` {
			t.Fatalf("JSON/Go-quoted form %s differs from the secret as sent", quoted)
		}
	}
}
