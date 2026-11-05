package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// clearCredentialEnv takes every launcher-env credential the probes read
// out of the test process, so "no credential" is a fact and not whatever
// this machine happens to export. t.Setenv(name, "") reads as absent on
// every probe path (LookupEnv value check / Getenv != "").
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_BASE_URL", "MOONSHOT_API_KEY",
		"KIMI_API_KEY", "ZAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY", "GOOGLE_API_KEY",
		"GROQ_API_KEY", "XAI_API_KEY", "OPENROUTER_API_KEY", "DEEPSEEK_API_KEY", "MISTRAL_API_KEY",
		"AI_GATEWAY_API_KEY", "OPENAI_COMPATIBLE_API_KEY",
		"ITERION_OPENAI_USE_OAUTH", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "ITERION_PI_AGENT_DIR",
	} {
		t.Setenv(name, "")
	}
}

// credentialAdmissionRun drives the PRODUCTION path — resolveAndStartSandbox
// — for a run with the given trust, on the canary driver whose Prepare
// refuses: errCanaryStop means "admitted past the credential gate", a
// *RuntimeError with ErrCodeSandboxCredentialUnreachable means the #1768
// refusal fired, before anything started.
func credentialAdmissionRun(t *testing.T, wf *ir.Workflow, trust store.RunTrust, events *[]store.Event) error {
	t.Helper()
	return credentialAdmissionRunCtx(t, context.Background(), wf, trust, events)
}

// credentialAdmissionRunCtx is credentialAdmissionRun on a context of the
// caller's — one carrying the run's credentials.
func credentialAdmissionRunCtx(t *testing.T, ctx context.Context, wf *ir.Workflow, trust store.RunTrust, events *[]store.Event) error {
	t.Helper()
	return credentialAdmissionRunRoutes(t, ctx, wf, trust, model.RouteView{}, events)
}

// credentialAdmissionStart is credentialAdmissionRunCtx under the routes the
// run's dispatch reads (its overrides, vars and provider allowlist).
func credentialAdmissionStart(t *testing.T, ctx context.Context, wf *ir.Workflow, trust store.RunTrust, routes model.RouteView) error {
	t.Helper()
	return credentialAdmissionRunRoutes(t, ctx, wf, trust, routes, nil)
}

func credentialAdmissionRunRoutes(t *testing.T, ctx context.Context, wf *ir.Workflow, trust store.RunTrust, routes model.RouteView, events *[]store.Event) error {
	t.Helper()
	return credentialAdmissionDetecting(t, ctx, wf, trust, routes, nil, events)
}

// credentialAdmissionDetecting is credentialAdmissionRunRoutes with the
// dispatch's pick for a claw node naming no model (SandboxParams.
// DetectedClawSpec); nil picks none.
func credentialAdmissionDetecting(t *testing.T, ctx context.Context, wf *ir.Workflow, trust store.RunTrust, routes model.RouteView, detected func(nodeID string) (string, error), events *[]store.Event) error {
	t.Helper()
	t.Setenv("ITERION_MODE", "local") // pin the factory to docker,podman,noop
	d := &specCapturingDriver{hostBinds: true}
	_, err := resolveAndStartSandbox(ctx, SandboxParams{
		DetectedClawSpec: detected,
		Routes:           routes,
		Workflow:         wf,
		RunID:            "run-credential-admission",
		Trust:            trust,
		WorkspacePath:    t.TempDir(),
		RepoRoot:         t.TempDir(),
		// A non-stock image keeps #1643's image admission out of the way:
		// this test judges the CREDENTIAL refusal, not the CLI-presence one.
		DefaultImage: "registry.example.test/operator-sandbox:latest",
		Drivers: map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		},
		Logger: iterlog.Nop(),
		EmitEvent: func(t store.EventType, data map[string]any) error {
			if events != nil {
				*events = append(*events, store.Event{Type: t, Data: data})
			}
			return nil
		},
	})
	return err
}

// credentialWorkflow is a sandboxed one-agent run whose node names backend,
// with host_state as given ("" = the auto default).
func credentialWorkflow(id, backend, hostState string) *ir.Workflow {
	return &ir.Workflow{
		Name: "credential_admission",
		Sandbox: &ir.SandboxSpec{
			Mode:      string(sandbox.ModeAuto),
			HostState: hostState,
		},
		Nodes: map[string]ir.Node{
			id: &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: id},
				LLMFields: ir.LLMFields{Backend: backend},
			},
		},
	}
}

// TestForkRunWithoutCredentialChannelIsRefusedBeforeTheSandboxStarts is the
// refusal of #1768: an untrusted run whose claude_code node no credential
// can reach — no run credential, no forwarded launcher env, and host_state
// withdrawn so the operator's file login does not mount — is refused TYPED,
// naming the backend, before the driver prepares anything.
//
// Mutants seen red: drop the admitSandboxCredentials call in
// resolveAndStartSandbox → errCanaryStop comes back instead of the refusal;
// drop the host_state input from the probe → the mounted-login arm below
// reddens.
func TestForkRunWithoutCredentialChannelIsRefusedBeforeTheSandboxStarts(t *testing.T) {
	clearCredentialEnv(t)
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) {
		t.Fatalf("resolveAndStartSandbox = %v, want a typed refusal — a fork run whose node gets no credential must not reach the pulls", err)
	}
	if errors.Is(err, errCanaryStop) {
		t.Fatal("the driver prepared the sandbox before the refusal: the admission is in the wrong place")
	}
	if rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("refusal code = %q, want %q", rerr.Code, ErrCodeSandboxCredentialUnreachable)
	}
	if !strings.Contains(rerr.Message, "claude_code") || !strings.Contains(rerr.Message, "review") {
		t.Fatalf("the refusal names the backend and the node: %q", rerr.Message)
	}
	if rerr.Hint == "" || !strings.Contains(rerr.Hint, "ANTHROPIC_API_KEY") {
		t.Fatalf("the refusal names the ways out: %q", rerr.Hint)
	}
}

// TestTrustedRunKeepsItsNoCredentialBehavior is the trusted control: the
// same shape on a trusted run is NOT the fork lane's business and admits
// exactly as before (the node may fail later on the backend's own "not
// logged in" — the operator's run, the operator's choice).
func TestTrustedRunKeepsItsNoCredentialBehavior(t *testing.T) {
	clearCredentialEnv(t)
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), "", nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("trusted run = %v, want admission (canary) — the credential refusal is the fork lane's, not a trusted run's", err)
	}
}

// TestForkRunWithAForwardedKeyAdmits is the fork arm that HAS a channel:
// the launcher's ANTHROPIC_API_KEY is forwarded by name at exec — the
// channel a spec-level check cannot see, and the reason the predicate
// reads the real spawn.
func TestForkRunWithAForwardedKeyAdmits(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-admission-witness-9f8e7d6c5b4a")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("fork run with a forwarded key = %v, want admission (canary)", err)
	}
}

// TestForkRunWithAForwardedAuthorizationHeaderAdmits: the launcher's
// ANTHROPIC_CUSTOM_HEADERS block is forwarded beside the token channels
// (ambientForwardedChannels), and an Authorization line in it is the
// credential the pod spends — the probe must name it, or a header-only pod
// is refused as credential-less on a run that authenticates fine. A block
// of other header names carries nothing the CLI authenticates with and
// stays a refusal — the strong reading, not the block's presence.
func TestForkRunWithAForwardedAuthorizationHeaderAdmits(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer gw-admission-secret")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("fork run with a forwarded header credential = %v, want admission (canary)", err)
	}
}

// TestForkRunWithANamelessHeaderBlockIsRefused is the block-presence
// control: no Authorization line, no credential — the run still has no
// channel and keeps the typed refusal.
func TestForkRunWithANamelessHeaderBlockIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Tenant: acme")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("fork run with a header block of no Authorization = %v, want the typed credential refusal", err)
	}
}

// TestForkRunWithAForwardedXAPIKeyHeaderAdmits: an x-api-key line in the
// forwarded block is applied over the key header the CLI's own auth sets, so
// it is the credential a header-only pod spends — the probe reads the set the
// ambient meter keys (every header the Anthropic wire's auth sets, not the
// Authorization line alone) and admits it.
func TestForkRunWithAForwardedXAPIKeyHeaderAdmits(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "x-api-key: gw-admission-key")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("fork run with a forwarded x-api-key header = %v, want admission (canary)", err)
	}
}

// TestForkRunWhoseOnlyTokenAnEmptyLineErasesIsRefused: an empty Authorization
// line is applied too — over the bearer the forwarded ANTHROPIC_AUTH_TOKEN
// sets — and the request then carries no credential at all. The token's
// presence is not a credential on the wire; the run keeps the typed refusal
// rather than dying "Not logged in" past the gate.
func TestForkRunWhoseOnlyTokenAnEmptyLineErasesIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-admission-erased")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization:")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("fork run whose only token an empty Authorization line erases = %v, want the typed credential refusal", err)
	}
}

// TestForkRunWithAnEmptyAuthorizationLineIsRefused is the value-less control:
// an Authorization line whose value is empty puts no credential on the
// request — the pod's only claim to one dies "Not logged in" at the node,
// past the gate. The probe answers on the usable-credential reading (the
// same one the ambient meter keys), so the run keeps the typed refusal.
func TestForkRunWithAnEmptyAuthorizationLineIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization:")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", "none"), store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("fork run with an empty-valued Authorization line = %v, want the typed credential refusal", err)
	}
}

// TestForkRunWithMountedLoginAdmits: while host_state is mounted (nothing
// on main floors it for an untrusted run), the operator's file-based login
// DOES reach a docker sandbox — refusing here would kill a run that works.
func TestForkRunWithMountedLoginAdmits(t *testing.T) {
	clearCredentialEnv(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claude_code", ""), store.RunTrustFork, nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("fork run with the login mounted = %v, want admission (canary)", err)
	}
}

// TestForkRunOnPiWithoutCredentialChannelIsRefused: pi's channels are its
// forwarded allowlist, the run's BYOK keys, and the codex bridge — none
// present here.
func TestForkRunOnPiWithoutCredentialChannelIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	err := credentialAdmissionRun(t, credentialWorkflow("review", "pi", "none"), store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("pi fork run = %v, want the typed credential refusal", err)
	}
}

// TestForkRunOnPiWithAForwardedKeyAdmits: the piSandboxEnv allowlist is the
// channel, measured against the real spawn's own list.
func TestForkRunOnPiWithAForwardedKeyAdmits(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("KIMI_API_KEY", "kimi-admission-witness-9f8e7d6c5b4a")
	err := credentialAdmissionRun(t, credentialWorkflow("review", "pi", "none"), store.RunTrustFork, nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("pi fork run with a forwarded key = %v, want admission (canary)", err)
	}
}

// TestForkRunOnClawWithoutCredentialChannelIsRefused: claw's channels are
// forwardableProviderEnv, the dispatch-time ChatGPT-forfait copy and the
// host_state mount — none present here.
func TestForkRunOnClawWithoutCredentialChannelIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	err := credentialAdmissionRun(t, credentialWorkflow("review", "claw", "none"), store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("claw fork run = %v, want the typed credential refusal", err)
	}
}

// TestForkRunOnClawReadsTheNodesProviderHint: a `provider: "anthropic"` claw
// node goes to Anthropic direct whatever the pod's ANTHROPIC_BASE_URL says,
// so its run's Claude forfait crosses the sandbox past a z.ai-redirected
// wire — the probe composes that crossing under the node's hint, as the
// dispatch does, and admits. The hint-less twin of the same node leaves the
// forfait home and has no channel.
func TestForkRunOnClawReadsTheNodesProviderHint(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	forfait := t.TempDir()
	if err := os.WriteFile(filepath.Join(forfait, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-run","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfait},
	})
	clawNode := func(provider string) *ir.Workflow {
		wf := credentialWorkflow("review", "claw", "none")
		node := wf.Nodes["review"].(*ir.AgentNode)
		node.Model = "anthropic/claude-sonnet-4"
		node.Provider = provider
		return wf
	}
	var rerr *RuntimeError
	if err := credentialAdmissionRunCtx(t, ctx, clawNode(""), store.RunTrustFork, nil); !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("bench: the hint-less claw node = %v, want the typed credential refusal — the forfait stays off a redirected wire", err)
	}
	if err := credentialAdmissionRunCtx(t, ctx, clawNode("anthropic"), store.RunTrustFork, nil); !errors.Is(err, errCanaryStop) {
		t.Fatalf("the hinted claw node = %v, want admission (canary) — its run's forfait crosses to Anthropic direct", err)
	}
}

// TestForkRunOnAnUnreadableBackendWarns: a backend the predicate cannot
// read is the warning's territory, never a refusal on a guess — the run
// admits, and sandbox_no_credential_warning says which nodes went unread.
func TestForkRunOnAnUnreadableBackendWarns(t *testing.T) {
	clearCredentialEnv(t)
	var events []store.Event
	err := credentialAdmissionRun(t, credentialWorkflow("review", "some_custom_backend", "none"), store.RunTrustFork, &events)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("undecidable backend = %v, want admission with a warning (canary)", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == store.EventSandboxNoCredentialWarning {
			found = true
		}
	}
	if !found {
		t.Fatal("no sandbox_no_credential_warning emitted for a backend the predicate cannot read")
	}
}
