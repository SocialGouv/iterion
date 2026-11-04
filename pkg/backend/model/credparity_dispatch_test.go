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
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestCredParity_Dispatch drives every row of ADR-161's parity table
// (internal/credparity) through the claw dispatch and judges the credential it
// spends against the row: the chain the executor walks (its own expansion,
// parse and allowlist), and per leg, Execute's subscription switch, then — on
// the host — the client the registry builds, or — in the sandbox — the env the
// crossing hands the container and the client the container's own registry
// builds from it, the forfait seeded where the runtime seeds it. The first leg
// that resolves spends; with none, the last leg's refusal is the node's.
//
// Serial: the registry's env factories read the process environment, so each
// row's pod is the process's for the row's duration.
func TestCredParity_Dispatch(t *testing.T) {
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	t.Cleanup(SetOAuthDirLookup(RunOAuthDirLookup))
	for _, row := range credparity.Rows() {
		t.Run(row.Label(), func(t *testing.T) {
			if row.Node.Backend != delegate.BackendClaw {
				t.Fatalf("no dispatch harness for backend %q: its arm's unit brings one", row.Node.Backend)
			}
			forfaitDir := applyParityRow(t, row)
			ctx := secrets.WithCredentials(context.Background(), row.Run.Credentials(forfaitDir))

			e := &ClawExecutor{allowedProviders: AllowedProviderSet(row.Run.Allowlist)}
			chain := e.resolveProviderChain(parityNode(row.Node))
			legs := make([]credparity.Leg, 0, len(chain))
			for _, el := range chain {
				legs = append(legs, credparity.Leg{Provider: el.Provider, Model: el.Model})
			}
			if !slices.Equal(legs, row.Node.Legs) {
				t.Fatalf("the executor walks %+v, the row states %+v — the row's legs are the dispatch's", legs, row.Node.Legs)
			}

			got := credparity.Refuses(credparity.OutsideAllowlist)
			for i := range chain {
				got = dispatchParityLeg(t, ctx, row, i, forfaitDir)
				if !got.Refused() {
					break
				}
			}
			credparity.Check(t, row, credparity.Dispatch, got, func(s credparity.Spend) credparity.Spend { return s })
		})
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

// dispatchParityLeg is one element's dispatch: Execute's switch guard, then
// the in-process resolution or the crossing and the container's.
func dispatchParityLeg(t *testing.T, ctx context.Context, row credparity.Row, leg int, forfaitDir string) credparity.Spend {
	t.Helper()
	spec, hint := row.Node.Spec(leg), row.Node.Hint(leg)
	if err := clawSubscriptionGuard(ctx, spec); err != nil {
		return parityRefusal(t, err)
	}
	if !row.Sandboxed {
		client, err := NewRegistry().ResolveWithContext(WithProviderHint(ctx, hint), spec)
		if err != nil {
			return parityRefusal(t, err)
		}
		return paritySpend(t, row, leg, client)
	}
	env, _, err := forwardableProviderEnv(ctx, spec, hint)
	if err != nil {
		return parityRefusal(t, err)
	}
	var got credparity.Spend
	t.Run(fmt.Sprintf("container leg %d", leg), func(t *testing.T) {
		// The container's process env is the crossed env and nothing else of
		// the pod's; the runtime seeds the run's forfait where the crossing
		// points CLAUDE_CONFIG_DIR.
		for _, name := range credparity.PostureNames {
			t.Setenv(name, "")
		}
		for _, name := range providerCredentialEnvVars {
			t.Setenv(name, "")
		}
		for name, v := range env {
			t.Setenv(name, v)
		}
		configDir := t.TempDir()
		if env["CLAUDE_CONFIG_DIR"] == secrets.ClaudeCodeSandboxConfigDir {
			if forfaitDir == "" {
				t.Fatal("the crossing seeds a forfait the run does not hold")
			}
			configDir = forfaitDir
		}
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		client, err := NewSandboxRunnerRegistry().ResolveWithContext(WithProviderHint(context.Background(), hint), spec)
		if err != nil {
			got = parityRefusal(t, err)
			return
		}
		got = paritySpend(t, row, leg, client)
	})
	return got
}

// paritySpend names the credential a built client sends by the channel that
// holds its value: the bearer it sends as its OAuth token, else its x-api-key
// — replaced by the Authorization line of the process's custom-header block
// where the client applies that block (claw's own identity pass, which the
// engine's clients refuse: NoAmbientHeaders).
func paritySpend(t *testing.T, row credparity.Row, leg int, client api.APIClient) credparity.Spend {
	t.Helper()
	cc, ok := client.(*api.Client)
	if !ok {
		t.Fatalf("leg %d built a %T: the harness reads an anthropic-wire client", leg, client)
	}
	cred := cc.OAuthToken
	if cred == "" {
		cred = cc.APIKey
	}
	if !cc.NoAmbientHeaders {
		if line, ok := credheader.LineValue(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), credheader.NameAuthorization); ok {
			cred = line
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

// parityRefusal names a dispatch refusal by the error its site returns.
func parityRefusal(t *testing.T, err error) credparity.Spend {
	t.Helper()
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
