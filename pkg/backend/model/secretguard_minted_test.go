package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// blankHostCredentials keeps the host's own credentials out of the guard a
// test builds: BuildSecretGuard registers every secret-named env var.
func blankHostCredentials(t *testing.T) {
	t.Helper()
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) {
			t.Setenv(name, "")
		}
	}
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// A credential the server minted for the run rides the launch vars; the guard
// scrubs it from every sink and hands it back nowhere: its placeholder is
// deterministic, so resolving it would give the value to whoever writes it.
func TestBuildSecretGuard_TheMintedGrantIsRedactOnly(t *testing.T) {
	blankHostCredentials(t)
	tok := randomToken(t)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	ph := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	if red := g.Redact("X-Iterion-Publish: " + tok); strings.Contains(red, tok) || !strings.Contains(red, ph) {
		t.Fatalf("Redact left the grant or named no placeholder: %q", red)
	}
	if got := g.Materialize("printf %s " + ph); strings.Contains(got, tok) {
		t.Error("Materialize resolved the grant's placeholder")
	}
	if got, env := g.MaterializeShellEnv("PUB_TOKEN='" + ph + "' publish"); strings.Contains(got, tok) || len(env) != 0 {
		t.Error("MaterializeShellEnv resolved the grant's placeholder")
	}
	if got := g.MaterializeShell("pub_token = '" + ph + "'"); strings.Contains(got, tok) {
		t.Error("MaterializeShell resolved the grant's placeholder")
	}
	if got := g.MaterializeForHost("Authorization: "+ph, "attacker.example"); strings.Contains(got, tok) {
		t.Error("MaterializeForHost resolved the grant's placeholder")
	}
	if (&ClawExecutor{secretGuard: g}).secretMaterializer() != nil {
		t.Error("a guard with nothing materialisable hands agents a materializer")
	}

	masked := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: store.RedactedLaunchVar}, nil)
	if masked != nil && masked.ContainsSecret(store.RedactedLaunchVar) {
		t.Error("the read surfaces' mask was registered as a secret value")
	}
}

// A lineage's records may hold several grants — a fork's source, an
// ancestor's, the run's own: the guard redacts every one, and resolves none.
func TestBuildSecretGuard_EveryRecordedGrantIsRedacted(t *testing.T) {
	blankHostCredentials(t)
	launch, own, ancestor := randomToken(t), randomToken(t), randomToken(t)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: launch}, map[string][]string{store.ForgePublishTokenVar: {own, ancestor}})
	red := g.Redact(strings.Join([]string{launch, own, ancestor}, " "))
	for name, tok := range map[string]string{"launch": launch, "own record": own, "ancestor": ancestor} {
		if strings.Contains(red, tok) {
			t.Errorf("the %s grant is not redacted: %q", name, red)
		}
	}
	if got := g.Materialize(secretguard.PlaceholderForName(store.ForgePublishTokenVar)); strings.Contains(got, own) || strings.Contains(got, ancestor) {
		t.Error("Materialize resolved a recorded grant's placeholder")
	}
}

// An agent that writes a placeholder gets the value only for a secret its
// workflow declares for it. The runner's own env, the run's provider keys and
// the minted grant stay placeholders in whatever the agent runs.
func TestAnAgentMaterializesOnlyTheSecretsItsWorkflowDeclares(t *testing.T) {
	blankHostCredentials(t)
	ambient := "Ambient-" + randomToken(t)
	provider := "sk-ant-" + randomToken(t)
	grant := randomToken(t)
	declared := "Declared-" + randomToken(t)
	t.Setenv("ITERION_SECRETS_KEY", ambient)
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.Provider("anthropic"): provider},
	})
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{"deploy_key": {Value: declared}}}
	guard := BuildSecretGuard(ctx, wf, map[string]string{store.ForgePublishTokenVar: grant}, nil)

	dir := t.TempDir()
	tr := tool.NewRegistry()
	if err := tool.RegisterClawAll(tr, tool.ClawDefaults{Workspace: dir}); err != nil {
		t.Fatal(err)
	}
	cmd := "printf '%s\\n' __ITERION_SECRET_env_ITERION_SECRETS_KEY__ __ITERION_SECRET_provider_key_0__ " +
		secretguard.PlaceholderForName(store.ForgePublishTokenVar) + " " + secretguard.PlaceholderForName("deploy_key") + " > exfil.txt"
	in, _ := json.Marshal(map[string]string{"command": cmd})
	mock := newMockClient(toolUseEvents("t1", "bash", string(in), 10, 10), textEvents("done", 10, 10))
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, wf, WithSecretGuard(guard), WithToolRegistry(tr), WithWorkDir(dir))
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "reviewer"}, LLMFields: ir.LLMFields{Model: "anthropic/claude-sonnet-4-6"}, Tools: []string{"bash"}}
	if _, err := exec.Execute(ctx, node, map[string]any{"diff": "untrusted pull request content"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "exfil.txt"))
	if err != nil {
		t.Fatalf("the agent's command did not run: %v", err)
	}
	got := string(raw)
	for what, value := range map[string]string{"the runner's ambient env": ambient, "the run's provider key": provider, "the minted grant": grant} {
		if strings.Contains(got, value) {
			t.Errorf("%s was materialised into the agent's command", what)
		}
	}
	if !strings.Contains(got, declared) {
		t.Error("the declared secret's placeholder no longer resolves for the agent")
	}
}

// The self-repair model sees the failed command, its stdout and its stderr —
// never the minted token in them, not even a prefix cut at the truncation
// bound. The command it proposes from the redacted text runs with the
// placeholder unresolved.
func TestSelfRepairNeverShowsTheModelAMintedToken(t *testing.T) {
	blankHostCredentials(t)
	// The self-repair rung refuses an unfunded default before dispatching;
	// this test's registry is a mock, so fund the wire nominally.
	t.Setenv("ANTHROPIC_API_KEY", "mock-funding")
	tok := randomToken(t)
	dir := t.TempDir()
	placeholder := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	corrected := fmt.Sprintf("printf '%%s' '%s' > repaired_marker", placeholder)
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "structured_output",
		fmt.Sprintf(`{"corrected_command":%q}`, corrected), 50, 10))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	guard := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(guard))

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "publish"},
		// stdout carries the token across the prompt's 4000-byte bound.
		Command:       "printf '%s' '" + strings.Repeat("x", 3970) + tok + "'; echo refused " + tok + " >&2",
		Goal:          "publish the review",
		Postcondition: "test -f repaired_marker",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxRepairAttempts: 1},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("Execute: %v — the corrected command did not run", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "repaired_marker"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) || strings.TrimSpace(string(raw)) != placeholder {
		t.Errorf("repaired_marker = %q, want the unresolved placeholder", raw)
	}
	calls := mock.getCalls()
	if len(calls) == 0 {
		t.Fatal("the self-repair model was never asked")
	}
	sawPlaceholder := false
	for _, call := range calls {
		for _, msg := range call.Messages {
			for _, block := range msg.Content {
				if strings.Contains(block.Text, tok[:24]) {
					t.Fatalf("the self-repair prompt carries the minted token (or a prefix of it): %q", block.Text)
				}
				sawPlaceholder = sawPlaceholder || strings.Contains(block.Text, placeholder)
			}
		}
	}
	if !sawPlaceholder {
		t.Error("the self-repair prompt does not show the placeholder in the token's place")
	}
}

// The acceptance of #2033, through the REAL executor: a tool node's own
// command names the grant, the tool process runs, and the bearer is nowhere
// in that process's /proc/<pid>/cmdline — which any co-resident process reads
// for the whole life of the command, and which Kubernetes copies into the
// pods/exec audit log. It still ARRIVES: the command reads it from its own
// environment, so the fix is not a silent loss of the publish capability.
func TestTheMintedGrantNeverEntersAToolProcessCmdline(t *testing.T) {
	blankHostCredentials(t)
	// `$$` is the pid of the bash the executor starts, whose argv holds the
	// resolved command; cat keeps the NUL separators for the reader below.
	const command = `cat /proc/$$/cmdline > cmdline.bin; printf '%s' {{vars.forge_publish_token}} > token.txt`
	refs, err := ir.ParseRefs(command)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := secretguard.PlaceholderForName(store.ForgePublishTokenVar)

	for _, tc := range []struct {
		name     string
		recorded map[string][]string
	}{
		// A fresh launch carries the grant in its launch vars; a resumed run
		// carries it in the checkpoint vars the engine resumes from, and its
		// RECORD may hold others (its own, an ancestor's, a fork's source).
		{name: "fresh launch"},
		{name: "resume", recorded: map[string][]string{store.ForgePublishTokenVar: {randomToken(t), randomToken(t)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := randomToken(t)
			dir := t.TempDir()
			wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
			vars := map[string]string{store.ForgePublishTokenVar: tok, store.ForgePublishURLVar: "https://iterion.example/publish"}
			guard := BuildSecretGuard(t.Context(), wf, vars, tc.recorded)
			e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(dir), WithSecretGuard(guard))
			e.vars = map[string]any{store.ForgePublishTokenVar: tok, store.ForgePublishURLVar: "https://iterion.example/publish"}

			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "publish"}, Command: command, CommandRefs: refs}
			if _, err := e.Execute(context.Background(), node, nil); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline.bin"))
			if err != nil {
				t.Fatalf("the tool process did not run: %v", err)
			}
			argv := strings.ReplaceAll(string(cmdline), "\x00", " ")
			if strings.Contains(argv, tok) {
				t.Errorf("the tool process's cmdline carries the grant: %q", argv)
			}
			for _, other := range tc.recorded[store.ForgePublishTokenVar] {
				if strings.Contains(argv, other) {
					t.Errorf("the tool process's cmdline carries a recorded grant: %q", argv)
				}
			}
			if !strings.Contains(argv, `"$`+placeholder+`"`) {
				t.Errorf("cmdline = %q, want the grant referenced through the environment", argv)
			}
			got, err := os.ReadFile(filepath.Join(dir, "token.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tok {
				// The run presents ITS grant: a lineage's other records are
				// registered for redaction, never for carriage.
				t.Errorf("the command received %d bytes, want the run's own grant: a publish would be refused", len(got))
			}
		})
	}
}

// The provenance rule, at the seam: the guard resolves the grant for text the
// AUTHOR wrote and for nothing else. A model's own command — the self-repair
// rung's corrected command — goes through the plain materialiser.
func TestOnlyAuthoredTextMaterializesTheMintedGrant(t *testing.T) {
	blankHostCredentials(t)
	tok := randomToken(t)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	ph := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	if got := g.AuthoredEnvPlaceholder(store.ForgePublishTokenVar); got != ph {
		t.Fatalf("AuthoredEnvPlaceholder = %q, want %q — the template layer would render the value itself", got, ph)
	}
	body := "publish --token '" + ph + "'"
	authored, env := g.MaterializeAuthoredShellEnv(body)
	if strings.Contains(authored, tok) {
		t.Errorf("the authored command inlines the grant: %q", authored)
	}
	if env[ph] != tok {
		t.Errorf("the authored command's env does not carry the grant (%d entries)", len(env))
	}
	if !strings.Contains(authored, `"$`+ph+`"`) {
		t.Errorf("authored = %q, want the environment reference", authored)
	}
	modelText, modelEnv := g.MaterializeShellEnv(body)
	if modelText != body || len(modelEnv) != 0 {
		t.Errorf("a model's command resolved the grant: %q env=%v", modelText, modelEnv)
	}
	// No inline fallback: a bare occurrence stays as written rather than
	// being substituted into the command line the whole class avoids.
	bare, bareEnv := g.MaterializeAuthoredShellEnv("publish --token " + ph)
	if strings.Contains(bare, tok) || len(bareEnv) != 0 {
		t.Errorf("a bare placeholder was inlined: %q env=%v", bare, bareEnv)
	}
}

// The grant is scoped to the endpoints the server bound it to, so the egress
// gate (Layer 2) refuses it toward anywhere else.
func TestTheMintedGrantIsScopedToItsEndpoints(t *testing.T) {
	blankHostCredentials(t)
	tok := randomToken(t)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{
		store.ForgePublishTokenVar: tok,
		store.ForgePublishURLVar:   "https://iterion.example/api/v1/forge/publish",
		store.ForgePRStateURLVar:   "https://iterion.example/api/v1/forge/pr-state",
	}, nil)
	if !g.ExfiltratesTo("Authorization: Bearer "+tok, "attacker.example") {
		t.Error("the grant leaves toward any host: the egress gate does not know its scope")
	}
	if g.ExfiltratesTo("Authorization: Bearer "+tok, "iterion.example") {
		t.Error("the grant is refused toward the endpoint it was minted for: the publish itself would fail")
	}
	// An endpoint we cannot read is no scope at all — refusing the publish
	// would be worse than the exposure.
	unscoped := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	if unscoped.ExfiltratesTo("Bearer "+tok, "attacker.example") {
		t.Error("a grant whose endpoints are unknown was scoped to nothing reachable")
	}
}

// Item 2 of #2033: a `script:` body is written to a temp file — pushed into
// the workspace by a copy-based sandbox — so an inlined bearer would sit in
// clear on disk for the node's lifetime. The body names the variable itself
// (iterion cannot rewrite a reference in someone else's language) and the
// engine hands the value to the interpreter: the script's own source, read
// back by the script, carries no grant.
func TestAScriptBodyReadsTheMintedGrantFromItsEnvironment(t *testing.T) {
	blankHostCredentials(t)
	tok := randomToken(t)
	ph := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	script := "cat \"$0\" > body.txt\nprintf '%s' \"$" + ph + "\" > token.txt\n"
	dir := t.TempDir()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	guard := BuildSecretGuard(t.Context(), wf, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(dir), WithSecretGuard(guard))
	e.vars = map[string]any{store.ForgePublishTokenVar: tok}

	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "publish"}, Script: script, Language: "sh"}
	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "body.txt"))
	if err != nil {
		t.Fatalf("the script did not run: %v", err)
	}
	if strings.Contains(string(body), tok) {
		t.Errorf("the script file on disk carries the grant: %q", body)
	}
	got, err := os.ReadFile(filepath.Join(dir, "token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != tok {
		t.Errorf("the script received %d bytes, want the grant: the publish would be skipped as ungranted", len(got))
	}
}

// The catalog's call-site shape, unchanged by the fix: every bot writes
// `NAME={{vars.forge_publish_token}} <program>` and reads NAME from its
// environment. The three names the catalog uses must all still arrive — a
// shape that stopped carrying the grant would turn the publish into a silent
// "no grant" skip, which is exactly what the bots report when it is absent.
func TestEveryCatalogCallSiteShapeStillCarriesTheGrant(t *testing.T) {
	blankHostCredentials(t)
	// PUB_TOKEN: review-pr, branch-improve-loop's emit, gitops-warden.
	// CHECK_AUTH: the delivery preflights. PR_STATE_TOKEN: the PR-state post.
	for _, name := range []string{"PUB_TOKEN", "CHECK_AUTH", "PR_STATE_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			tok := randomToken(t)
			command := name + `={{vars.forge_publish_token}} sh -c 'printf %s "$` + name + `" > token.txt'; cat /proc/$$/cmdline > cmdline.bin`
			refs, err := ir.ParseRefs(command)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
			guard := BuildSecretGuard(t.Context(), wf, map[string]string{store.ForgePublishTokenVar: tok}, nil)
			e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(dir), WithSecretGuard(guard))
			e.vars = map[string]any{store.ForgePublishTokenVar: tok}

			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "publish"}, Command: command, CommandRefs: refs}
			if _, err := e.Execute(context.Background(), node, nil); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "token.txt"))
			if err != nil {
				t.Fatalf("the call site did not run: %v", err)
			}
			if string(got) != tok {
				t.Errorf("%s received %d bytes, want the grant", name, len(got))
			}
			cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), tok) {
				t.Errorf("%s's cmdline carries the grant", name)
			}
		})
	}
}
