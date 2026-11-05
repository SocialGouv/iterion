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
	"github.com/SocialGouv/iterion/pkg/backend/detect"
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

			node := row.Node.IR()
			e := &ClawExecutor{allowedProviders: AllowedProviderSet(row.Run.Allowlist), detector: detect.NewCachedDetector(0)}
			view := RouteView{Workflow: &ir.Workflow{Name: "parity", Nodes: map[string]ir.Node{node.NodeID(): node}}, AllowedProviders: row.Run.Allowlist}
			site := paritySite(t, view, row)
			legs := Legs(view, node)
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
				if site.Host {
					got = dispatchParityDirect(t, ctx, row, site)
				} else {
					got = dispatchParityLeg(t, ctx, row, site, e, i, legs[i], forfaitDir)
				}
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

// paritySite is the row's call site as the one enumeration of the run's
// model-calling sites names it (CallSites) — the site the admission judges
// and the pre-flight keys.
func paritySite(t *testing.T, v RouteView, row credparity.Row) CallSite {
	t.Helper()
	for _, s := range CallSites(v, nil) {
		if s.ID == row.Node.SiteID() {
			return s
		}
	}
	t.Fatalf("CallSites names no site %q for the row's node", row.Node.SiteID())
	return CallSite{}
}

// dispatchParityDirect is a direct generation's dispatch (a human node's LLM
// half, a Verified Action's rung 3): the claw arm's in-process resolution the
// generation builds its client from (resolveBeside, under the hint-free ctx
// those generations run on), which must be the answer the enumeration's call
// gets from the registered arm, then the credential the client sends.
func dispatchParityDirect(t *testing.T, ctx context.Context, row credparity.Row, site CallSite) credparity.Spend {
	t.Helper()
	spec := site.Model
	client, spend, err := NewRegistry().resolveBeside(ctx, spec)
	armSpend, armErr := credroute.Resolve(ctx, site.Call(0, spec, credroute.Sandbox{Contained: row.Sandboxed}))
	if armSpend != spend || (armErr == nil) != (err == nil) {
		t.Fatalf("the enumeration's call answers %+v, %v; the generation's own resolution %+v, %v — one arm, one answer", armSpend, armErr, spend, err)
	}
	if err != nil {
		return parityRefusal(t, err)
	}
	return paritySpend(t, row, 0, client)
}

// dispatchParityLeg is one element's dispatch: the claw arm's resolution of
// the leg, then the credential the dispatch sends on it — the client the
// resolution built in process, or the client the container's own registry
// builds from the env the resolution crossed. The arm's answer must name that
// credential, or the refusal the dispatch meets.
func dispatchParityLeg(t *testing.T, ctx context.Context, row credparity.Row, site CallSite, e *ClawExecutor, i int, leg credroute.Leg, forfaitDir string) credparity.Spend {
	t.Helper()
	spec := row.Node.Spec(i)
	if spec == "" {
		// A claw dispatch naming no model: an agent node builds its task on
		// the detector's pick (buildTask), an llm router sends its default
		// (executeLLMRouterUnified's routerModel).
		picked, err := parityDispatchedBase(e, site.Node)
		if err != nil {
			return parityRefusal(t, err)
		}
		spec = picked
	}
	// The enumeration names the spec the dispatch sends: the admission and
	// the pre-flight judge the call the dispatch makes.
	if base, berr := site.Base(e.DetectedClawSpec); berr != nil || site.LegSpec(i, base) != spec {
		t.Fatalf("leg %d: the enumeration's spec is %q (base %q, %v); the dispatch sends %q", i, site.LegSpec(i, base), base, berr, spec)
	}
	call := credroute.Call{
		Backend: delegate.BackendClaw, Spec: spec, Leg: leg, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: row.Sandboxed},
	}
	if row.Role == credparity.SupervisorRole {
		// A supervisor's evaluator resolves its model in process, on the
		// host, whatever the run's sandbox.
		call.Role, call.Sandbox = credroute.RoleSupervisor, credroute.Sandbox{}
	} else if enumerated := site.Call(i, spec, credroute.Sandbox{Contained: row.Sandboxed}); enumerated != call {
		t.Fatalf("leg %d: the enumeration asks %+v, the dispatch %+v — the admission and the pre-flight would judge a call the dispatch never makes", i, enumerated, call)
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
	case err != nil && (!crossesRefused || (res.client == nil && res.env == nil)):
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
			// The runner's own call (ClawBackend.Execute in the container: the
			// arm in process over the runner's registry, built on the
			// crossing's record): the client it sends with, and the Spend it
			// names itself — the keys its events and its result carry, which
			// the launcher relays verbatim. In process and sandboxed agree:
			// that Spend is the arm's answer for the contained call, the one
			// the admission, the meter and the ledger read.
			container := NewSandboxRunnerRegistry(res.facts.funds)
			client, own, cerr := inProcessClaw(context.Background(), container, credroute.Call{
				Backend: delegate.BackendClaw, Spec: spec, Leg: leg, Role: call.Role,
			})
			if cerr != nil {
				got = parityRefusal(t, cerr)
				return
			}
			if own != armSpend {
				t.Errorf("leg %d: the container names its call %+v, the arm %+v — the keys the runner writes are not what the readers read", i, own, armSpend)
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

// parityDispatchedBase is the spec a claw dispatch of node naming no model
// sends, read by the dispatch's own reading: an llm router's model
// (chainReading.routerModel, which executeLLMRouterUnified sends), any other
// node's detector pick (DetectedClawSpec, which buildTask sends).
func parityDispatchedBase(e *ClawExecutor, node ir.Node) (string, error) {
	if r, ok := node.(*ir.RouterNode); ok {
		spec, _ := e.chainReading().routerModel(r)
		return spec, nil
	}
	return e.DetectedClawSpec(node.NodeID())
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
