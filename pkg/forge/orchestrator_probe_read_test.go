package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// TestForgeRejectedTheWrite_AStepEndingOnAReadAppliedNothing: the classifier
// decides whether a re-provision may put its previous webhook config back, and
// the probe of the delivery URL is a GET. Reading its 5xx as a write of unknown
// outcome keeps the fresh config on a forge that never received the secret,
// which is every delivery to a working repo answering 401 — so the read is
// typed, and a read's status is not a write's.
func TestForgeRejectedTheWrite_AStepEndingOnAReadAppliedNothing(t *testing.T) {
	probe := func(cause error, priorWrite error) error {
		return &hookReadError{op: "probe hook by URL", err: cause, priorWrite: priorWrite}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{{
		name: "the probe answered 502",
		err:  probe(StatusErr("gitlab", "get hooks", http.StatusBadGateway), nil),
		want: true,
	}, {
		name: "the probe answered 503",
		err:  probe(StatusErr("gitlab", "get hooks", http.StatusServiceUnavailable), nil),
		want: true,
	}, {
		name: "the probe timed out with no answer",
		err:  probe(transportErr(errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")), nil),
		want: true,
	}, {
		name: "the probe answered 422 (a project at its hook ceiling)",
		err:  probe(StatusErr("gitlab", "get hooks", http.StatusUnprocessableEntity), nil),
		want: true,
	}, {
		// The real recreate path: the stored hook id is stale, the update
		// answers 404 — a settled answer — and the probe after it fails.
		name: "the probe failed after an update the forge answered 404",
		err:  probe(StatusErr("gitlab", "get hooks", http.StatusBadGateway), ErrHookNotFound),
		want: true,
	}, {
		// A read cannot settle what a write before it left open: the step
		// stays indeterminate, whatever the read answered.
		name: "the probe failed after a write whose outcome is unknown",
		err:  probe(StatusErr("gitlab", "get hooks", http.StatusUnprocessableEntity), StatusErr("gitlab", "update hook", http.StatusBadGateway)),
		want: false,
	}, {
		name: "a hook WRITE answered 502",
		err:  StatusErr("gitlab", "update hook", http.StatusBadGateway),
		want: false,
	}, {
		name: "a hook WRITE answered 422",
		err:  StatusErr("gitlab", "update hook", http.StatusUnprocessableEntity),
		want: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := forgeRejectedTheWrite(tc.err); got != tc.want {
				t.Errorf("forgeRejectedTheWrite(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestReprovision_ProbeOutageKeepsTheWorkingRepo is the scenario end to end: a
// working repo is re-provisioned (an admin adds a bot, or pins a webhook base),
// the stored hook id no longer matches the forge's — an import renumbered the
// hooks — and the probe that follows the 404 hits a gateway answering 502. No
// write was sent, so the previous config goes back and the repo keeps
// authenticating; the operator is told the step that failed, not to re-run a
// provision that already converged.
func TestReprovision_ProbeOutageKeepsTheWorkingRepo(t *testing.T) {
	bg := context.Background()
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	f := &matrixForge{}
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }

	res := provisionBots(t, o, "review-pr")
	live := f.only(t)
	prevCfg, err := o.Webhooks.Get(bg, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}

	staleHookID(t, o)
	f.failStep = "get"
	f.failErr = StatusErr("gitlab", "get hooks", http.StatusBadGateway)

	_, perr := o.Provision(bg, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	if perr == nil {
		t.Fatal("the probe failure did not surface")
	}
	var indeterminate *HookWriteIndeterminateError
	if errors.As(perr, &indeterminate) {
		t.Errorf("a failed READ registers nothing, so the write is not of unknown outcome: %v", perr)
	}
	if !strings.Contains(perr.Error(), "probe hook by URL") {
		t.Errorf("the error does not name the step that failed: %v", perr)
	}
	if !errors.Is(perr, f.failErr) {
		t.Errorf("the forge's own failure is no longer reachable: %v", perr)
	}

	cfg, err := o.Webhooks.Get(bg, res.WebhookID)
	if err != nil {
		t.Fatalf("the failed re-provision left no stored webhook config at all: %v", err)
	}
	if cfg.TokenHash != prevCfg.TokenHash {
		t.Errorf("the stored config carries this attempt's secret although the forge never received it")
	}
	if !webhooks.VerifyToken(live.secret, cfg.TokenHash) {
		t.Errorf("the forge's live hook signs with a secret the stored config rejects — every delivery to this "+
			"working repo answers 401\n  forge hooks: %+v\n  error: %v", f.hooks, perr)
	}
	if got := f.only(t); got != live {
		t.Errorf("the forge-side hook changed to %+v, want the untouched %+v", got, live)
	}
}

// TestReprovision_Probe422IsNotARefusedAddress: a 422 is how GitLab answers a
// project that has reached its hook ceiling, and a hook LISTING offered the
// forge no address. Typing it as a refused delivery URL answers
// hook_url_refused and sends the operator to pin a webhook base for a call
// that registered nothing.
func TestReprovision_Probe422IsNotARefusedAddress(t *testing.T) {
	bg := context.Background()
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	f := &matrixForge{}
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }

	res := provisionBots(t, o, "review-pr")
	live := f.only(t)

	staleHookID(t, o)
	f.failStep = "get"
	f.failErr = &StatusError{Provider: "gitlab", Op: "get hooks", Code: http.StatusUnprocessableEntity,
		Detail: "Maximum number of project hooks (100) exceeded"}

	_, perr := o.Provision(bg, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	if perr == nil {
		t.Fatal("the probe failure did not surface")
	}
	var refused *HookRefusedError
	if errors.As(perr, &refused) {
		t.Errorf("a failed hook LISTING is typed *HookRefusedError (URL=%s): the route answers hook_url_refused "+
			"and sends the operator to pin a webhook base for a read that registered nothing\n  error: %v",
			refused.URL, perr)
	}
	if !strings.Contains(perr.Error(), "Maximum number of project hooks (100) exceeded") {
		t.Errorf("the forge's own reason is gone, and it is the only one that names the real limit: %v", perr)
	}
	cfg, err := o.Webhooks.Get(bg, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !webhooks.VerifyToken(live.secret, cfg.TokenHash) {
		t.Errorf("the previous config is owed back after a read that applied nothing: %v", perr)
	}
}

// TestReprovision_CredentialStepFailureKeepsTheWorkingRepo: on a GitHub App
// connection the hook write is two calls — the installation-token mint, then
// the hook request — and a cold client mints inside the write. A mint that
// fails (whatever it answered: a 502, or a 422 that names the token request)
// sent no hook request, so the previous config goes back and the repo keeps
// authenticating; and the 422 of that step is no refused address.
func TestReprovision_CredentialStepFailureKeepsTheWorkingRepo(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			bg := context.Background()
			o, _, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			f := &matrixForge{}
			o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }

			res := provisionBots(t, o, "review-pr")
			live := f.only(t)
			prevCfg, err := o.Webhooks.Get(bg, res.WebhookID)
			if err != nil {
				t.Fatal(err)
			}

			f.failStep = "update"
			f.failErr = &HookNotSentError{Step: "mint installation token", Err: StatusErr("github", "mint installation token", code)}

			_, perr := o.Provision(bg, ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"revi-converse"}, ActorID: "u1",
			})
			if perr == nil {
				t.Fatal("the credential failure did not surface")
			}
			var indeterminate *HookWriteIndeterminateError
			if errors.As(perr, &indeterminate) {
				t.Errorf("no hook request was sent, so the write is not of unknown outcome: %v", perr)
			}
			var refused *HookRefusedError
			if errors.As(perr, &refused) {
				t.Errorf("a 422 of the token step refuses no address: %v", perr)
			}
			cfg, err := o.Webhooks.Get(bg, res.WebhookID)
			if err != nil {
				t.Fatalf("the failed re-provision left no stored webhook config at all: %v", err)
			}
			if cfg.TokenHash != prevCfg.TokenHash || !webhooks.VerifyToken(live.secret, cfg.TokenHash) {
				t.Errorf("the forge's untouched hook signs with a secret the stored config rejects — every delivery to this working repo answers 401: %v", perr)
			}
		})
	}
}
