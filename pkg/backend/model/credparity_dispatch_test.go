package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/internal/credheader"
	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestCredParity_Dispatch drives every row of ADR-161's parity table
// (internal/credparity) through the claw dispatch and judges the credential it
// spends against the row: the chain the executor walks (model.Legs, the
// executor's own resolveProviderChain), and per leg the claw arm — the
// registered one every reader asks (credroute.Resolve) and the dispatch's own
// resolution over its registry (resolveClawCall), which must agree — then the
// credential the dispatch actually sends: on the host the client the
// resolution built, in the sandbox the client the container's own registry
// builds from the env the resolution crossed, the forfait seeded where the
// runtime seeds it. The arm's Spend names that credential's channel and slot,
// or its refusal is the one the dispatch meets. The first leg that resolves
// spends; with none, the last leg's refusal is the node's.
//
// Serial: the registry's env factories read the process environment, so each
// row's pod is the process's for the row's duration.
func TestCredParity_Dispatch(t *testing.T) {
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	t.Cleanup(SetOAuthDirLookup(RunOAuthDirLookup))
	if !slices.Contains(credroute.Registered(), delegate.BackendClaw) {
		t.Fatalf("no claw arm is registered with the resolver (registered: %v)", credroute.Registered())
	}
	judged := 0
	for _, row := range credparity.Rows() {
		if credparity.DispatchHarness(row.Node.Backend) != credparity.ModelHarness {
			continue // its dispatch is driven where its backend lives
		}
		judged++
		t.Run(row.Label(), func(t *testing.T) {
			forfaitDir := applyParityRow(t, row)
			ctx := secrets.WithCredentials(context.Background(), row.Run.Credentials(forfaitDir))

			node := parityNode(row.Node)
			e := &ClawExecutor{allowedProviders: AllowedProviderSet(row.Run.Allowlist)}
			legs := Legs(RouteView{AllowedProviders: row.Run.Allowlist}, node)
			if executor := legsOf(e.resolveProviderChain(node)); !slices.Equal(executor, legs) {
				t.Fatalf("the executor walks %+v, model.Legs reads %+v — one chain, one reading", executor, legs)
			}
			want := make([]credroute.Leg, 0, len(row.Node.Legs))
			for _, l := range row.Node.Legs {
				want = append(want, credroute.Leg{Provider: l.Provider, Model: l.Model})
			}
			if !slices.Equal(legs, want) {
				t.Fatalf("model.Legs reads %+v, the row states %+v — the row's legs are the dispatch's", legs, row.Node.Legs)
			}

			got := credparity.Refuses(credparity.OutsideAllowlist)
			for i := range legs {
				got = dispatchParityLeg(t, ctx, row, i, legs[i], forfaitDir)
				if !got.Refused() {
					break
				}
			}
			credparity.Check(t, row, credparity.Dispatch, got, func(s credparity.Spend) credparity.Spend { return s })
		})
	}
	if judged == 0 {
		t.Fatal("inert harness: no row of the table is a claw row")
	}
}

// applyParityRow blanks every credential name the readers read, applies the
// row's posture, points CLAUDE_CONFIG_DIR at a login-less dir (claw reads the
// host's Claude Code login last) and writes the run's forfait. It returns the
// forfait's dir, "" when the run holds none.
func applyParityRow(t *testing.T, row credparity.Row) string {
	t.Helper()
	for _, name := range credparity.PostureNames {
		t.Setenv(name, "")
	}
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	for name, v := range row.Posture {
		t.Setenv(name, v)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if !row.Run.Forfait {
		return ""
	}
	dir := t.TempDir()
	if err := credparity.WriteForfait(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func parityNode(n credparity.Node) ir.Node {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: n.ID},
		LLMFields: ir.LLMFields{Backend: n.Backend, Model: n.Model, Provider: n.Provider},
	}
}

// dispatchParityLeg is one element's dispatch: the claw arm's resolution of
// the leg, then the credential the dispatch sends on it — the client the
// resolution built in process, or the client the container's own registry
// builds from the env the resolution crossed. The arm's answer must name that
// credential, or the refusal the dispatch meets.
func dispatchParityLeg(t *testing.T, ctx context.Context, row credparity.Row, i int, leg credroute.Leg, forfaitDir string) credparity.Spend {
	t.Helper()
	spec, hint := row.Node.Spec(i), row.Node.Hint(i)
	call := credroute.Call{
		Backend: delegate.BackendClaw, Spec: spec, Leg: leg, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: row.Sandboxed},
	}
	if row.Role == credparity.SupervisorRole {
		// A supervisor's evaluator resolves its model in process, on the
		// host, whatever the run's sandbox.
		call.Role, call.Sandbox = credroute.RoleSupervisor, credroute.Sandbox{}
	}
	armSpend, armErr := credroute.Resolve(ctx, call)
	res, err := resolveClawCall(ctx, NewRegistry(), call)
	if armSpend != res.spend || (armErr == nil) != (err == nil) {
		t.Fatalf("leg %d: the registered arm answers %+v, %v; the dispatch's resolution %+v, %v — one arm, one answer", i, armSpend, armErr, res.spend, err)
	}
	var refusal *credroute.Refusal
	crossesRefused := errors.As(err, &refusal) && refusal.Reason == credroute.ReasonNoCredential
	var got credparity.Spend
	switch {
	case err != nil && !(crossesRefused && (res.client != nil || res.env != nil)):
		got = parityRefusal(t, err)
	case !call.Sandbox.Contained:
		got = paritySpend(t, row, i, res.client)
	default:
		t.Run(fmt.Sprintf("container leg %d", i), func(t *testing.T) {
			// The container's process env is the crossed env and nothing else
			// of the pod's; the runtime seeds the run's forfait where the
			// crossing points CLAUDE_CONFIG_DIR.
			for _, name := range credparity.PostureNames {
				t.Setenv(name, "")
			}
			for _, name := range providerCredentialEnvVars {
				t.Setenv(name, "")
			}
			for name, v := range res.env {
				t.Setenv(name, v)
			}
			configDir := t.TempDir()
			if res.env["CLAUDE_CONFIG_DIR"] == secrets.ClaudeCodeSandboxConfigDir {
				if forfaitDir == "" {
					t.Fatal("the crossing seeds a forfait the run does not hold")
				}
				configDir = forfaitDir
			}
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			client, cerr := NewSandboxRunnerRegistry().ResolveWithContext(WithProviderHint(context.Background(), hint), spec)
			if cerr != nil {
				got = parityRefusal(t, cerr)
				return
			}
			got = paritySpend(t, row, i, client)
		})
	}
	// The arm names what the dispatch sends: its channel and slot, or the
	// reason it is refused.
	if got.Refused() {
		if armErr == nil || parityRefusal(t, armErr) != got {
			t.Errorf("leg %d: the dispatch is refused (%s), the arm answers %+v, %v", i, got.Refusal, armSpend, armErr)
		}
	} else if armErr != nil || string(armSpend.Channel) != string(got.Channel) || armSpend.Slot != got.Slot {
		t.Errorf("leg %d: the dispatch spends the %s (slot %q), the arm answers %+v, %v", i, got.Channel, got.Slot, armSpend, armErr)
	}
	return got
}

// paritySpend names the credential a built client sends by the channel that
// holds its value: the bearer it sends as its OAuth token, else its key —
// on the anthropic wire replaced by the Authorization line of the process's
// custom-header block where the client applies that block (claw's own
// identity pass, which the engine's clients refuse: NoAmbientHeaders). A
// client of claw's openai provider (the xai route's) is read by its fields'
// names: its type lives in claw's internal package.
func paritySpend(t *testing.T, row credparity.Row, leg int, client api.APIClient) credparity.Spend {
	t.Helper()
	var cred string
	if cc, ok := client.(*api.Client); ok {
		cred = cc.OAuthToken
		if cred == "" {
			cred = cc.APIKey
		}
		if !cc.NoAmbientHeaders {
			if line, ok := credheader.LineValue(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), credheader.NameAuthorization); ok {
				cred = line
			}
		}
	} else {
		key, token := openAIClientAuth(t, client)
		cred = token
		if cred == "" {
			cred = key
		}
	}
	switch {
	case cred == "":
		return credparity.Refuses(credparity.NoCredential)
	case cred == credparity.ForfaitToken && row.Run.Forfait:
		return credparity.Spent(leg, credparity.RunForfait, string(secrets.OAuthKindClaudeCode), cred)
	}
	for p, k := range row.Run.Keys {
		if k == cred {
			return credparity.Spent(leg, credparity.RunKey, string(p), cred)
		}
	}
	for name, v := range row.Posture {
		if v == cred || name == "ANTHROPIC_CUSTOM_HEADERS" && cred == credparity.HeaderBearer {
			return credparity.Spent(leg, credparity.PodEnv, "", cred)
		}
	}
	t.Fatalf("leg %d sends a credential no channel of the row holds", leg)
	return credparity.Spend{}
}

// parityRefusal names a dispatch refusal by the error its site returns: the
// resolver's typed reason, else the site's own error.
func parityRefusal(t *testing.T, err error) credparity.Spend {
	t.Helper()
	if r := (*credroute.Refusal)(nil); errors.As(err, &r) {
		return credparity.Refuses(credparity.Reason(r.Reason))
	}
	var ambient *ErrAmbientCredentialRefused
	var unfunded *ErrProviderHintUnfunded
	var unavailable *delegate.ErrModelUnavailable
	switch {
	case errors.As(err, &ambient):
		return credparity.Refuses(credparity.AmbientStamp)
	case errors.Is(err, secrets.ErrSubscriptionOAuthForbidden):
		return credparity.Refuses(credparity.SubscriptionSwitch)
	case errors.As(err, &unfunded):
		return credparity.Refuses(credparity.HintUnfunded)
	case errors.Is(err, ErrProviderOutsideAllowlist):
		return credparity.Refuses(credparity.OutsideAllowlist)
	case errors.As(err, &unavailable):
		return credparity.Refuses(credparity.NoCredential)
	}
	t.Fatalf("an unclassified dispatch refusal: %v", err)
	return credparity.Spend{}
}
