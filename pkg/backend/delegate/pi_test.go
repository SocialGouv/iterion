package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestPiResolveModel(t *testing.T) {
	cases := []struct {
		name         string
		model, hint  string
		wantProvider string
		wantModel    string
	}{
		{name: "bare id lets pi resolve", model: "gpt-5.5", wantProvider: "", wantModel: "gpt-5.5"},
		{name: "known prefix passes through", model: "anthropic/claude-opus-4-8", wantProvider: "anthropic", wantModel: "claude-opus-4-8"},
		{name: "gemini alias", model: "gemini/gemini-3-pro", wantProvider: "google", wantModel: "gemini-3-pro"},
		{name: "vertex alias", model: "vertex/gemini-3-pro", wantProvider: "google-vertex", wantModel: "gemini-3-pro"},
		{name: "bedrock alias", model: "bedrock/claude-sonnet-4-6", wantProvider: "amazon-bedrock", wantModel: "claude-sonnet-4-6"},
		{name: "azure alias", model: "azure/gpt-5.5", wantProvider: "azure-openai-responses", wantModel: "gpt-5.5"},
		{name: "kimi alias", model: "kimi/kimi-k2", wantProvider: "moonshotai", wantModel: "kimi-k2"},
		{name: "unknown prefix passes through verbatim", model: "cerebras/llama-4", wantProvider: "cerebras", wantModel: "llama-4"},
		{name: "hint alone", model: "gpt-5.5", hint: "groq", wantProvider: "groq", wantModel: "gpt-5.5"},

		// The z.ai landmine: iterion routes z.ai through the Anthropic-
		// compatible surface, so the SPEC says anthropic while the hint says
		// zai. pi has a first-class zai provider — taking the spec's prefix
		// would fuzzy-match Anthropic's own catalogue and silently run a
		// different model.
		{name: "hint overrides spec prefix (z.ai)", model: "anthropic/glm-5.2", hint: "zai", wantProvider: "zai", wantModel: "glm-5.2"},

		{name: "trailing slash is not a prefix", model: "weird/", wantProvider: "", wantModel: "weird/"},
		{name: "empty", model: "", wantProvider: "", wantModel: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotProvider, gotModel := piResolveModel(tc.model, tc.hint)
			if gotProvider != tc.wantProvider || gotModel != tc.wantModel {
				t.Errorf("piResolveModel(%q,%q) = (%q,%q), want (%q,%q)",
					tc.model, tc.hint, gotProvider, gotModel, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

func TestPiMapEffort(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		"low":       {"--thinking", "low"},
		"medium":    {"--thinking", "medium"},
		"high":      {"--thinking", "high"},
		"xhigh":     {"--thinking", "xhigh"},
		"max":       {"--thinking", "max"},
		"ultracode": {"--thinking", "xhigh"}, // pi has no subagent tool
		"  HIGH  ":  {"--thinking", "high"},
	}
	for in, want := range cases {
		if got := piMapEffort(in); !reflect.DeepEqual(got, want) {
			t.Errorf("piMapEffort(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPiExtraArgsFor(t *testing.T) {
	t.Run("provider and model emitted together", func(t *testing.T) {
		args := piExtraArgsFor(Task{Model: "anthropic/claude-opus-4-8"}, nil)
		if i := slices.Index(args, "--provider"); i < 0 || args[i+1] != "anthropic" {
			t.Fatalf("missing --provider anthropic in %v", args)
		}
		if i := slices.Index(args, "--model"); i < 0 || args[i+1] != "claude-opus-4-8" {
			t.Fatalf("missing --model claude-opus-4-8 in %v", args)
		}
	})

	t.Run("bare model omits provider", func(t *testing.T) {
		args := piExtraArgsFor(Task{Model: "gpt-5.5"}, nil)
		if slices.Contains(args, "--provider") {
			t.Fatalf("unexpected --provider for a bare model: %v", args)
		}
	})

	// pi mints its own id for a fresh session and reports it in the stream
	// header. Pinning --session-id for a session that does not exist yet
	// works but makes pi warn on EVERY first run — observed live.
	t.Run("fresh session is left to pi", func(t *testing.T) {
		args := piExtraArgsFor(Task{Model: "gpt-5.5"}, nil)
		if slices.Contains(args, "--session-id") {
			t.Errorf("args = %v, want no --session-id for a fresh session "+
				"(pi warns that the id does not exist yet)", args)
		}
		if !slices.Contains(args, "--session-dir") {
			t.Errorf("args = %v, want --session-dir so sessions stay with the run", args)
		}
	})

	t.Run("resume uses session-id, fork uses fork", func(t *testing.T) {
		resume := piExtraArgsFor(Task{SessionID: "s1"}, nil)
		if i := slices.Index(resume, "--session-id"); i < 0 || resume[i+1] != "s1" {
			t.Errorf("resume args = %v, want --session-id s1", resume)
		}
		fork := piExtraArgsFor(Task{SessionID: "s1", ForkSession: true}, nil)
		if i := slices.Index(fork, "--fork"); i < 0 || fork[i+1] != "s1" {
			t.Errorf("fork args = %v, want --fork s1", fork)
		}
		if slices.Contains(fork, "--session-id") {
			t.Errorf("fork must not also pin --session-id: %v", fork)
		}
	})

	t.Run("session dir never the operator's pi home", func(t *testing.T) {
		args := piExtraArgsFor(Task{StoreDir: "/store"}, nil)
		i := slices.Index(args, "--session-dir")
		if i < 0 {
			t.Fatalf("missing --session-dir in %v", args)
		}
		if got := args[i+1]; got != filepath.Join("/store", "pi", "sessions") {
			t.Errorf("session dir = %q, want it under the run store", got)
		}
	})

	// Only what the ENGINE reports is offered — see TestPiSkillArgs. The
	// workspace is an untrusted checkout, so its contents prove nothing.
	t.Run("only engine-reported skills are passed", func(t *testing.T) {
		if none := piExtraArgsFor(Task{WorkDir: "/w"}, nil); slices.Contains(none, "--skill") {
			t.Errorf("unexpected --skill with nothing reported: %v", none)
		}

		work := t.TempDir()
		skill := filepath.Join(work, ".claude", "skills", "s")
		if err := os.MkdirAll(skill, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		some := piExtraArgsFor(Task{WorkDir: work, MirroredSkills: []string{skill}}, nil)
		i := slices.Index(some, "--skill")
		if i < 0 || some[i+1] != skill {
			t.Errorf("args = %v, want --skill pointing at the mirrored skill", some)
		}
	})

	t.Run("readonly is the only enforced tool gate", func(t *testing.T) {
		rw := piExtraArgsFor(Task{Model: "m"}, nil)
		if slices.Contains(rw, "--tools") {
			t.Errorf("unexpected --tools on a writable node: %v", rw)
		}
		ro := piExtraArgsFor(Task{Model: "m", Readonly: true}, nil)
		i := slices.Index(ro, "--tools")
		if i < 0 || strings.Contains(ro[i+1], "bash") || strings.Contains(ro[i+1], "write") {
			t.Errorf("args = %v, want a read-only tool set", ro)
		}
	})

	t.Run("offline only under sandbox, with an escape hatch", func(t *testing.T) {
		if slices.Contains(piExtraArgsFor(Task{Model: "m"}, nil), "--offline") {
			t.Error("--offline must not be forced on a host run")
		}
		sandboxed := Task{Model: "m", Sandbox: &recordingRun{}}
		if !slices.Contains(piExtraArgsFor(sandboxed, nil), "--offline") {
			t.Error("--offline expected under sandbox (catalogue refresh would stall on an egress policy)")
		}
		t.Setenv("ITERION_PI_OFFLINE", "0")
		if slices.Contains(piExtraArgsFor(sandboxed, nil), "--offline") {
			t.Error("ITERION_PI_OFFLINE=0 must disable --offline")
		}
	})

	// Context files stay on for parity with claude_code, but they are the
	// dominant per-call cost on a repo with a large CLAUDE.md (measured:
	// 26,933 input tokens vs 448 on iterion's own tree), so the off switch
	// must exist and must be off by default.
	t.Run("context files on by default, with an off switch", func(t *testing.T) {
		if slices.Contains(piExtraArgsFor(Task{}, nil), "--no-context-files") {
			t.Error("context files must stay on by default (claude_code parity)")
		}
		t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
		if !slices.Contains(piExtraArgsFor(Task{}, nil), "--no-context-files") {
			t.Error("ITERION_PI_NO_CONTEXT_FILES=1 must suppress AGENTS.md/CLAUDE.md injection")
		}
	})

	t.Run("project trust is opt-in", func(t *testing.T) {
		if slices.Contains(piExtraArgsFor(Task{}, nil), "--approve") {
			t.Error("target-repo .pi/ resources must not be trusted by default")
		}
		t.Setenv("ITERION_PI_TRUST_PROJECT", "1")
		if !slices.Contains(piExtraArgsFor(Task{}, nil), "--approve") {
			t.Error("ITERION_PI_TRUST_PROJECT=1 must opt into project trust")
		}
	})
}

// TestPiProtocolNeverPassesPromptAsArg is the regression guard for pi's
// argv parser silently dropping a message that starts with "-" or "@".
func TestPiProtocolNeverPassesPromptAsArg(t *testing.T) {
	b := &CLIAgentBackend{Protocol: piProtocol, Logger: testLogger()}
	prompt := "- Fix the failing test\n- Then report"
	args, stdin := b.buildArgs(piProtocol, Task{UserPrompt: prompt}, prompt, "")

	if slices.Contains(args, "-p") || slices.Contains(args, "--prompt") {
		t.Fatalf("prompt must never travel as an argv value: %v", args)
	}
	if slices.Contains(args, prompt) {
		t.Fatalf("prompt leaked into argv: %v", args)
	}
	if stdin != prompt {
		t.Fatalf("stdin = %q, want the prompt verbatim", stdin)
	}
}

func TestPiResolveEnv(t *testing.T) {
	t.Run("injects per-run BYOK keys", func(t *testing.T) {
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{
				secrets.ProviderOpenAI: "sk-openai",
				secrets.ProviderZAI:    "zai-token",
			},
		})
		env := piResolveEnv(ctx)
		if env["OPENAI_API_KEY"] != "sk-openai" {
			t.Errorf("OPENAI_API_KEY = %q, want sk-openai", env["OPENAI_API_KEY"])
		}
		if env["ZAI_API_KEY"] != "zai-token" {
			t.Errorf("ZAI_API_KEY = %q, want zai-token", env["ZAI_API_KEY"])
		}
	})

	// An inherited subscription OAuth token must reach pi: Anthropic accepts
	// it from a third-party app and bills it against extra usage. Stripping it
	// would only break a working credential path.
	t.Run("subscription oauth token is not stripped", func(t *testing.T) {
		env := piResolveEnv(context.Background())
		for _, key := range []string{"ANTHROPIC_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"} {
			if _, ok := env[key]; ok {
				t.Errorf("%s is overridden — an inherited subscription token would not reach pi", key)
			}
		}
	})

	// The opt-out has to reach the variable that actually carries the
	// subscription. ANTHROPIC_AUTH_TOKEN is the documented Anthropic
	// subscription bearer AND is forwarded into the sandbox by
	// piCredentialEnvNames, so omitting it left ITERION_FORBID_SUBSCRIPTION_OAUTH
	// a no-op on both paths — an incomplete refusal the operator believes in.
	t.Run("opt-out clears the anthropic subscription bearer", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat-SUBSCRIPTION")

		env := piResolveEnv(context.Background())
		if got, ok := env["ANTHROPIC_AUTH_TOKEN"]; !ok || got != "" {
			t.Errorf("ANTHROPIC_AUTH_TOKEN = %q (set=%v), want cleared under the opt-out", got, ok)
		}
		for _, key := range []string{"ANTHROPIC_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"} {
			if got, ok := env[key]; !ok || got != "" {
				t.Errorf("%s = %q (set=%v), want cleared under the opt-out", key, got, ok)
			}
		}
	})

	// The same variable carries the z.ai Anthropic-compatible facade key and
	// gateway bearers. The opt-out is about subscription SPEND, so it must key
	// on the token's shape — revoking a z.ai key would be an unrelated break.
	t.Run("opt-out leaves a non-subscription auth token alone", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "zai-facade-key")

		env := piResolveEnv(context.Background())
		if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
			t.Error("a z.ai/gateway bearer was cleared by the subscription opt-out")
		}
	})

	// Regression: a container inherits NOTHING from the host, so a provider
	// credential sitting in the host environment is invisible to a sandboxed
	// pi unless forwarded by name. The symptom was a sandboxed node failing
	// with "No API key found for zai" while the host had the key — observed
	// live, on the first real container run.
	t.Run("sandbox forwards host provider credentials by name", func(t *testing.T) {
		t.Setenv("ZAI_API_KEY", "zai-host-key")
		t.Setenv("GROQ_API_KEY", "groq-host-key")
		t.Setenv("SOME_UNRELATED_SECRET", "must-not-leak")

		env := piSandboxEnv(context.Background(), Task{
			ExtraEnv: []string{"PATH=/devbox/bin:/usr/bin"},
		})

		if env["ZAI_API_KEY"] != "zai-host-key" {
			t.Errorf("ZAI_API_KEY = %q, want the host value forwarded", env["ZAI_API_KEY"])
		}
		if env["GROQ_API_KEY"] != "groq-host-key" {
			t.Errorf("GROQ_API_KEY = %q, want the host value forwarded", env["GROQ_API_KEY"])
		}
		// The allowlist is the point: a blanket os.Environ() forward would push
		// every unrelated host secret into the container.
		if _, leaked := env["SOME_UNRELATED_SECRET"]; leaked {
			t.Error("an unrelated host secret leaked into the container environment")
		}
		// The run's own provisioning must survive too, or a sandboxed agent
		// cannot see tools the run just installed for it.
		if env["PATH"] != "/devbox/bin:/usr/bin" {
			t.Errorf("PATH = %q, want the run's devbox provisioning", env["PATH"])
		}
	})

	t.Run("agent dir pinning is opt-in", func(t *testing.T) {
		if _, ok := piResolveEnv(context.Background())["PI_CODING_AGENT_DIR"]; ok {
			t.Error("pinning the agent dir by default would hide the operator's own auth.json")
		}
		t.Setenv("ITERION_PI_AGENT_DIR", "/pinned")
		if got := piResolveEnv(context.Background())["PI_CODING_AGENT_DIR"]; got != "/pinned" {
			t.Errorf("PI_CODING_AGENT_DIR = %q, want /pinned", got)
		}
	})
}

func TestPiSubscriptionOAuthNotice(t *testing.T) {
	forfaitOnly := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{
			string(secrets.OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
		},
	})
	b := NewPiBackend(testLogger(), "")

	// Anthropic accepts a subscription token from a third-party app, billing
	// it against a separate extra-usage balance. So this proceeds — with a
	// warning, because the operator is spending a different pot than the plan.
	t.Run("anthropic on the subscription proceeds", func(t *testing.T) {
		if err := b.noticeSubscriptionOAuth(forfaitOnly, Task{Model: "anthropic/claude-opus-4-8"}); err != nil {
			t.Fatalf("unexpected refusal: Anthropic permits third-party apps (billed to extra usage): %v", err)
		}
	})

	// The opt-out exists for shared deployments, where spending an operator's
	// extra-usage balance is a decision taken on everyone's behalf.
	t.Run("opt-out refuses", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		if err := b.noticeSubscriptionOAuth(forfaitOnly, Task{Model: "anthropic/claude-opus-4-8"}); err == nil {
			t.Fatal("expected a refusal under ITERION_FORBID_SUBSCRIPTION_OAUTH=1")
		}
	})

	t.Run("opt-out spares a metered key", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		withKey := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-key"},
			OAuthCredentialFiles: map[string]string{
				string(secrets.OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
			},
		})
		if err := b.noticeSubscriptionOAuth(withKey, Task{Model: "anthropic/claude-opus-4-8"}); err != nil {
			t.Fatalf("unexpected refusal with a metered key: %v", err)
		}
	})

	t.Run("non-anthropic providers are untouched", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		if err := b.noticeSubscriptionOAuth(forfaitOnly, Task{Model: "openai/gpt-5.5"}); err != nil {
			t.Fatalf("unexpected refusal on a non-anthropic model: %v", err)
		}
	})

	// z.ai rides the Anthropic-compatible surface, so its SPEC says anthropic
	// while the hint says zai. It must not be mistaken for the subscription.
	t.Run("z.ai on the anthropic surface is not the subscription", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		if err := b.noticeSubscriptionOAuth(forfaitOnly, Task{Model: "anthropic/glm-5.2", ProviderHint: "zai"}); err != nil {
			t.Fatalf("z.ai must not be caught by the Anthropic subscription check: %v", err)
		}
	})
}

const piHappyStream = `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-27T10:00:00Z","cwd":"/w"}
{"type":"agent_start"}
{"type":"message_end","message":{"role":"assistant","model":"gpt-5.5","responseId":"r1","timestamp":1,"content":[{"type":"text","text":"partial"}],"stopReason":"toolUse","usage":{"input":100,"output":10,"cacheRead":5,"cacheWrite":0,"totalTokens":110,"cost":{"input":0.1,"output":0.02,"cacheRead":0,"cacheWrite":0,"total":0.12}}}}
{"type":"agent_end","willRetry":false,"messages":[{"role":"user","content":"hi","timestamp":0},{"role":"assistant","model":"gpt-5.5","responseModel":"gpt-5.5-2026","responseId":"r1","timestamp":1,"content":[{"type":"text","text":"partial"}],"stopReason":"toolUse","usage":{"input":100,"output":10,"cacheRead":5,"cacheWrite":0,"totalTokens":110,"cost":{"input":0.1,"output":0.02,"cacheRead":0,"cacheWrite":0,"total":0.12}}},{"role":"assistant","model":"gpt-5.5","responseId":"r2","timestamp":2,"content":[{"type":"text","text":"{\"answer\":\"42\"}"}],"stopReason":"stop","usage":{"input":200,"output":30,"cacheRead":50,"cacheWrite":10,"reasoning":12,"totalTokens":230,"cost":{"input":0.2,"output":0.06,"cacheRead":0,"cacheWrite":0,"total":0.26}}}]}
{"type":"agent_settled"}`

func TestParsePiOutput(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		got := parsePiOutput(piHappyStream)
		if got.Err != nil {
			t.Fatalf("unexpected Err: %v", got.Err)
		}
		if got.SessionID != "sess-1" {
			t.Errorf("SessionID = %q, want sess-1", got.SessionID)
		}
		if got.Text != `{"answer":"42"}` {
			t.Errorf("Text = %q, want the LAST assistant message", got.Text)
		}
		if got.InputTokens != 300 || got.OutputTokens != 40 {
			t.Errorf("tokens = (%d,%d), want (300,40)", got.InputTokens, got.OutputTokens)
		}
		if got.ThinkingTokens != 12 {
			t.Errorf("ThinkingTokens = %d, want 12", got.ThinkingTokens)
		}
		// Reasoning is a subset of output — it must not inflate the total.
		if got.OutputTokens < got.ThinkingTokens {
			t.Errorf("thinking (%d) must be a subset of output (%d)", got.ThinkingTokens, got.OutputTokens)
		}
		if got.CostUSD < 0.379 || got.CostUSD > 0.381 {
			t.Errorf("CostUSD = %v, want ~0.38", got.CostUSD)
		}
		// input+cacheRead+cacheWrite of the heaviest message: 200+50+10.
		if got.PeakInputTokens != 260 {
			t.Errorf("PeakInputTokens = %d, want 260", got.PeakInputTokens)
		}
		if got.EffectiveModel != "gpt-5.5" {
			t.Errorf("EffectiveModel = %q, want gpt-5.5 (the last message reports no override)", got.EffectiveModel)
		}
	})

	// The regression this fixture exists for: message_update re-emits the
	// SAME message on every streaming delta. Accumulating it would multiply
	// the bill by the delta count.
	t.Run("message_update deltas never double-count", func(t *testing.T) {
		stream := `{"type":"message_update","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"a"}],"usage":{"input":100,"output":5,"totalTokens":105,"cost":{"total":0.1}}}}
{"type":"message_update","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"ab"}],"usage":{"input":100,"output":5,"totalTokens":105,"cost":{"total":0.1}}}}
{"type":"message_end","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"ab"}],"stopReason":"stop","usage":{"input":100,"output":5,"totalTokens":105,"cost":{"total":0.1}}}}`
		got := parsePiOutput(stream)
		if got.InputTokens != 100 || got.OutputTokens != 5 {
			t.Errorf("tokens = (%d,%d), want (100,5) counted exactly once", got.InputTokens, got.OutputTokens)
		}
		if got.CostUSD < 0.099 || got.CostUSD > 0.101 {
			t.Errorf("CostUSD = %v, want 0.1 counted exactly once", got.CostUSD)
		}
	})

	t.Run("a discarded attempt is ignored in favour of the real one", func(t *testing.T) {
		stream := `{"type":"agent_end","willRetry":true,"messages":[{"role":"assistant","responseId":"bad","content":[{"type":"text","text":"boom"}],"stopReason":"error","errorMessage":"overloaded","usage":{"input":9,"output":9,"totalTokens":18,"cost":{"total":9}}}]}
{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","responseId":"good","content":[{"type":"text","text":"ok"}],"stopReason":"stop","usage":{"input":1,"output":1,"totalTokens":2,"cost":{"total":0.01}}}]}`
		got := parsePiOutput(stream)
		if got.Err != nil {
			t.Fatalf("unexpected Err from a retried-then-succeeded run: %v", got.Err)
		}
		if got.Text != "ok" || got.InputTokens != 1 {
			t.Errorf("got %+v, want only the surviving transcript", got)
		}
	})

	// pi's --mode json exits 0 even on a failed turn, so stopReason is the
	// only failure signal there is.
	t.Run("failure typing", func(t *testing.T) {
		cases := []struct {
			name, stopReason, errMsg string
			check                    func(*testing.T, error)
		}{
			{
				name: "rate limit", stopReason: "error", errMsg: "You've hit your usage limit. Resets 3pm.",
				check: func(t *testing.T, err error) {
					var rl *ErrRateLimited
					if !errors.As(err, &rl) {
						t.Fatalf("err = %v (%T), want *ErrRateLimited", err, err)
					}
					if rl.Provider != BackendPi {
						t.Errorf("Provider = %q, want %q", rl.Provider, BackendPi)
					}
				},
			},
			{
				name: "network", stopReason: "error", errMsg: "connection reset by peer",
				check: func(t *testing.T, err error) {
					var tr *ErrTransient
					if !errors.As(err, &tr) {
						t.Fatalf("err = %v (%T), want *ErrTransient", err, err)
					}
				},
			},
			{
				name: "aborted", stopReason: "aborted",
				check: func(t *testing.T, err error) {
					var tr *ErrTransient
					if !errors.As(err, &tr) {
						t.Fatalf("err = %v (%T), want *ErrTransient", err, err)
					}
				},
			},
			{
				name: "auth is deterministic, not retryable", stopReason: "error", errMsg: "invalid x-api-key",
				check: func(t *testing.T, err error) {
					var tr *ErrTransient
					var rl *ErrRateLimited
					if errors.As(err, &tr) || errors.As(err, &rl) {
						t.Fatalf("err = %v, want a plain error so retries are not burnt", err)
					}
					if err == nil {
						t.Fatal("expected an error")
					}
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				msg := map[string]any{
					"role": "assistant", "responseId": "r", "stopReason": tc.stopReason,
					"content": []any{map[string]any{"type": "text", "text": "partial"}},
				}
				if tc.errMsg != "" {
					msg["errorMessage"] = tc.errMsg
				}
				line, _ := json.Marshal(map[string]any{
					"type": "agent_end", "willRetry": false, "messages": []any{msg},
				})
				got := parsePiOutput(string(line))
				tc.check(t, got.Err)
			})
		}
	})

	// The upstream HTTP status pi records in diagnostics is the precise
	// signal, and it must beat any reading of the message text.
	t.Run("http status classification", func(t *testing.T) {
		cases := []struct {
			status int
			// wantKind: "ratelimit" | "transient" | "plain"
			wantKind string
		}{
			{429, "ratelimit"},
			{503, "transient"},
			{500, "transient"},
			{408, "transient"},
			{401, "plain"},
			{403, "plain"},
			{402, "plain"},
			{400, "plain"},
		}
		for _, tc := range cases {
			t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
				line, _ := json.Marshal(map[string]any{
					"type": "agent_end", "willRetry": false,
					"messages": []any{map[string]any{
						"role": "assistant", "responseId": "r", "stopReason": "error",
						// Deliberately bland text: the status must be what decides.
						"errorMessage": "request failed",
						"diagnostics": []any{map[string]any{
							"type": "provider_error", "timestamp": 1,
							"error": map[string]any{"message": "request failed", "code": tc.status},
						}},
					}},
				})
				err := parsePiOutput(string(line)).Err
				var rl *ErrRateLimited
				var tr *ErrTransient
				switch tc.wantKind {
				case "ratelimit":
					if !errors.As(err, &rl) {
						t.Fatalf("status %d → %v (%T), want *ErrRateLimited", tc.status, err, err)
					}
				case "transient":
					if !errors.As(err, &tr) {
						t.Fatalf("status %d → %v (%T), want *ErrTransient", tc.status, err, err)
					}
				default:
					if errors.As(err, &rl) || errors.As(err, &tr) {
						t.Fatalf("status %d → %v, want a plain (non-retried) error", tc.status, err)
					}
					if err == nil {
						t.Fatalf("status %d → nil, want an error", tc.status)
					}
				}
			})
		}
	})

	// Regression: a provider-shaped rate-limit message with no diagnostics.
	// The shared claude_code detector is deliberately narrow (it reads
	// untrusted assistant prose) and does not match this, which would
	// misclassify a throttle as permanent and skip retry + fallback.
	t.Run("provider-shaped rate limit without a status", func(t *testing.T) {
		for _, msg := range []string{
			"rate_limit_error: 429 too many requests",
			"Error: too many requests",
			"overloaded_error: server is overloaded",
		} {
			line, _ := json.Marshal(map[string]any{
				"type": "agent_end", "willRetry": false,
				"messages": []any{map[string]any{
					"role": "assistant", "responseId": "r",
					"stopReason": "error", "errorMessage": msg,
				}},
			})
			err := parsePiOutput(string(line)).Err
			var rl *ErrRateLimited
			if !errors.As(err, &rl) {
				t.Errorf("%q → %v (%T), want *ErrRateLimited", msg, err, err)
			}
		}
	})

	// Observed live: Anthropic ACCEPTS a subscription OAuth token from a
	// third-party app but bills it against a separate extra-usage balance.
	// An empty balance is a generic 400 whose text mentions nothing about
	// credentials, so without a translation it reads like a broken token.
	t.Run("anthropic extra-usage exhaustion is legible", func(t *testing.T) {
		upstream := `400 {"type":"error","error":{"type":"invalid_request_error",` +
			`"message":"Third-party apps now draw from your extra usage, not your plan limits. ` +
			`Add more at claude.ai/settings/usage and keep going."}}`
		line, _ := json.Marshal(map[string]any{
			"type": "agent_end", "willRetry": false,
			"messages": []any{map[string]any{
				"role": "assistant", "responseId": "r",
				"stopReason": "error", "errorMessage": upstream,
			}},
		})
		err := parsePiOutput(string(line)).Err
		if err == nil {
			t.Fatal("expected an error")
		}
		// Deterministic: a human must top the balance up. Retrying or
		// failing over to another provider cannot resolve it.
		var tr *ErrTransient
		var rl *ErrRateLimited
		if errors.As(err, &tr) || errors.As(err, &rl) {
			t.Errorf("err = %v (%T), want a plain error — retries cannot fix an empty balance", err, err)
		}
		for _, want := range []string{"extra-usage", "claude.ai/settings/usage", "claude_code"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error message lacks %q, so the operator cannot act on it: %v", want, err)
			}
		}
	})

	t.Run("unrecognisable output falls back to raw stdout", func(t *testing.T) {
		got := parsePiOutput("pi: command not found\n")
		if got.Text != "pi: command not found\n" {
			t.Errorf("Text = %q, want the raw stream so the schema fallback can still try", got.Text)
		}
	})

	t.Run("empty output", func(t *testing.T) {
		if got := parsePiOutput(""); got.Text != "" || got.Err != nil {
			t.Errorf("got %+v, want a zero parse", got)
		}
	})
}

func TestDefaultRegistryIncludesPi(t *testing.T) {
	b, err := DefaultRegistry(testLogger()).Resolve(BackendPi)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", BackendPi, err)
	}
	if _, ok := b.(*PiBackend); !ok {
		t.Fatalf("Resolve(%q) = %T, want *PiBackend", BackendPi, b)
	}
}

func TestPiSystemPromptMode(t *testing.T) {
	// pi has its own agentic prompt, AGENTS.md/CLAUDE.md loading and skills;
	// replacing it would strip the per-tool guidelines it assembles.
	if got := SystemPromptModeForBackend(BackendPi); got != SystemPromptAppendToNative {
		t.Errorf("SystemPromptModeForBackend(pi) = %v, want SystemPromptAppendToNative", got)
	}
}

// TestPiExecuteEndToEnd drives the backend against a fake `pi` that emits a
// print-mode stream, asserting argv, the system-prompt file, stdout parsing,
// structured-output extraction and cost annotation.
func TestPiExecuteEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	workDir := t.TempDir()
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "fakepi")
	argvDump := filepath.Join(binDir, "argv")
	stdinDump := filepath.Join(binDir, "stdin")

	script := `#!/bin/sh
printf '%s\n' "$@" > ` + argvDump + `
cat > ` + stdinDump + `
printf '%s\n' '{"type":"session","version":3,"id":"sess-9","timestamp":"t","cwd":"/w"}'
printf '%s\n' '{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","model":"gpt-5.5","responseId":"r1","content":[{"type":"text","text":"{\"answer\":\"42\"}"}],"stopReason":"stop","usage":{"input":120,"output":30,"totalTokens":150,"cost":{"total":0.5}}}]}'
printf '%s\n' '{"type":"agent_settled"}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}

	// This exercises the PRINT transport specifically — the fake CLI emits a
	// print-mode stream, and RPC (now the default) would time out on its
	// handshake against it.
	t.Setenv("ITERION_PI_MODE", "print")

	b := NewPiBackend(testLogger(), fake)
	task := Task{
		NodeID:           "review",
		WorkDir:          workDir,
		BaseDir:          workDir,
		SystemPrompt:     "be terse",
		SystemPromptMode: SystemPromptAppendToNative,
		UserPrompt:       "- what is the answer",
		Model:            "openai/gpt-5.5",
		ReasoningEffort:  "high",
		OutputSchema:     json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
	}
	res, err := b.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if res.BackendName != BackendPi {
		t.Errorf("BackendName = %q, want %q", res.BackendName, BackendPi)
	}
	if res.SessionID != "sess-9" {
		t.Errorf("SessionID = %q, want sess-9", res.SessionID)
	}
	if res.Output["answer"] != "42" {
		t.Errorf("Output[answer] = %v, want 42", res.Output["answer"])
	}
	if res.Tokens != 150 {
		t.Errorf("Tokens = %d, want 150 (real input+output split)", res.Tokens)
	}
	// The provider's own figure must win over iterion's estimate table.
	if got := res.Output["_cost_usd"]; got != 0.5 {
		t.Errorf("_cost_usd = %v, want 0.5 (pi's provider-computed cost)", got)
	}

	rawStdin, err := os.ReadFile(stdinDump) // #nosec G304 — test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawStdin), "- what is the answer") {
		t.Errorf("stdin = %q, want the prompt delivered on stdin", rawStdin)
	}

	rawArgv, err := os.ReadFile(argvDump) // #nosec G304 — test fixture path
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(rawArgv)), "\n")
	for _, want := range []string{"--mode", "json", "--no-approve", "--thinking", "high", "--provider", "openai", "--model", "gpt-5.5"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv missing %q: %v", want, argv)
		}
	}

	// The composed system prompt travels as a path, and the file is cleaned
	// up so it never shows in the run's diff.
	i := slices.Index(argv, "--append-system-prompt")
	if i < 0 {
		t.Fatalf("missing --append-system-prompt: %v", argv)
	}
	promptPath := argv[i+1]
	if !strings.HasPrefix(promptPath, workDir) || !strings.HasSuffix(promptPath, ".sysprompt.md") {
		t.Errorf("system prompt arg = %q, want a workspace-relative file path", promptPath)
	}
	if _, err := os.Stat(promptPath); !os.IsNotExist(err) {
		t.Errorf("system-prompt file %q survived the run", promptPath)
	}
}

// A permission-gated node must not run ungated. Every iterion capability on pi
// lives in the embedded extension, which only the RPC transport loads — so the
// documented ITERION_PI_MODE=print rollback would otherwise turn a node's
// `permission: ask|deny` block into a silent no-op, dropping the whole
// anti-prompt-injection boundary on an env-var flip. The RPC path already
// treats this combination as fatal; print must agree.
func TestPiPrintModeRefusesAPermissionGatedNode(t *testing.T) {
	t.Setenv("ITERION_PI_MODE", "print")

	pol, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	// A command that cannot exist, so a backend that WRONGLY proceeds fails
	// with an exec error instead of accidentally passing.
	b := NewPiBackend(testLogger(), filepath.Join(t.TempDir(), "no-such-pi"))

	_, err = b.Execute(context.Background(), Task{
		NodeID:     "gated",
		Model:      "anthropic/claude-sonnet-4-6",
		Permission: pol,
	})
	if err == nil {
		t.Fatal("print mode ran a permission-gated node — the gate would be INACTIVE")
	}
	if !strings.Contains(err.Error(), "permission gate") {
		t.Errorf("error = %v, want it to name the permission gate as the reason", err)
	}
}

// The gate is the only capability that fails rather than degrades: an ungated
// node still does useful work without ask_user or the board.
func TestPiPrintModeAllowsAnUngatedNode(t *testing.T) {
	t.Setenv("ITERION_PI_MODE", "print")

	b := NewPiBackend(testLogger(), filepath.Join(t.TempDir(), "no-such-pi"))
	_, err := b.Execute(context.Background(), Task{
		NodeID:             "plain",
		Model:              "anthropic/claude-sonnet-4-6",
		InteractionEnabled: true,
		Capabilities:       []string{"board.create"},
	})
	if err != nil && strings.Contains(err.Error(), "permission gate") {
		t.Errorf("an ungated node was refused by the gate guard: %v", err)
	}
}

// A sandboxed pi run keeps its sessions inside the worktree (the container has
// to see them), which makes them untracked files in the TARGET repo. Left
// alone they flip workdirIsClean and ride finalizeWorktree's `git add -A` into
// a wip-bank commit, so a run that changed no code still lands a commit full
// of pi transcripts — and scatters iterion's `.iterion/` into someone else's
// tree. Same self-ignoring guard devbox uses for its generated profile.
func TestPiHidesWorkspaceSessionDirFromGit(t *testing.T) {
	t.Run("workspace-relative dir is self-ignored", func(t *testing.T) {
		work := t.TempDir()
		piHideWorkspaceSessionDir(Task{WorkDir: work, Sandbox: &recordingRun{}}, testLogger())

		got, err := os.ReadFile(filepath.Join(work, ".iterion", ".gitignore"))
		if err != nil {
			t.Fatalf("no .gitignore guard written: %v", err)
		}
		if strings.TrimSpace(string(got)) != "*" {
			t.Errorf("guard = %q, want `*` so the guard ignores itself too", got)
		}
	})

	t.Run("an operator's existing gitignore is never overwritten", func(t *testing.T) {
		work := t.TempDir()
		if err := os.MkdirAll(filepath.Join(work, ".iterion"), 0o755); err != nil {
			t.Fatal(err)
		}
		guard := filepath.Join(work, ".iterion", ".gitignore")
		if err := os.WriteFile(guard, []byte("!keep-me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		piHideWorkspaceSessionDir(Task{WorkDir: work, Sandbox: &recordingRun{}}, testLogger())

		got, _ := os.ReadFile(guard)
		if strings.TrimSpace(string(got)) != "!keep-me" {
			t.Errorf("existing .gitignore was clobbered: %q", got)
		}
	})

	// The default NON-sandboxed path puts sessions in the store — but the
	// extension bundle and the composed system prompt still go to
	// <WorkDir>/.iterion/pi/, so the guard is needed there too. Keying it on
	// the session dir left exactly those two files exposed to a `git add -A`.
	t.Run("guards the workspace even when sessions live in the store", func(t *testing.T) {
		work, store := t.TempDir(), t.TempDir()
		piHideWorkspaceSessionDir(Task{WorkDir: work, StoreDir: store}, testLogger())

		got, err := os.ReadFile(filepath.Join(work, ".iterion", ".gitignore"))
		if err != nil {
			t.Fatalf("no guard for the extension bundle / system prompt: %v", err)
		}
		if strings.TrimSpace(string(got)) != "*" {
			t.Errorf("guard = %q, want `*`", got)
		}
	})

	t.Run("no write without a workspace", func(t *testing.T) {
		piHideWorkspaceSessionDir(Task{StoreDir: t.TempDir()}, testLogger())
	})
}

// The container's environment is ONLY what the spawner passes: pisdk ignores
// ClientOptions.Env whenever a Spawner is set. Forwarding just the provider
// credentials left the container with no ITERION_PI_CONTRACT, so the
// extension's loadConfig() returned early and registered NOTHING — and since
// sandboxing is the default, a node declaring `permission: ask|deny` ran
// completely ungated on the default path, with no error and no warning.
func TestPiSandboxedSpawnCarriesTheExtensionContract(t *testing.T) {
	rec := &recordingRun{}
	pol, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	task := Task{
		NodeID:     "gated",
		Iteration:  2,
		WorkDir:    t.TempDir(),
		Sandbox:    rec,
		Permission: pol,
	}

	b := &PiRPCBackend{Command: "pi", Logger: testLogger()}
	spawn := b.spawner(task, "pi", sandboxDelegateMark(task))
	if spawn == nil {
		t.Fatal("no spawner for a sandboxed task")
	}
	_ = spawn(context.Background(), []string{"--mode", "rpc"})

	env := rec.gotOpts.Env
	if env["ITERION_PI_CONTRACT"] == "" {
		t.Error("ITERION_PI_CONTRACT missing — the extension would register nothing " +
			"and the permission gate would be INACTIVE inside the container")
	}
	if env["ITERION_PI_NODE_ID"] != "gated" {
		t.Errorf("ITERION_PI_NODE_ID = %q, want the node id", env["ITERION_PI_NODE_ID"])
	}
	// The credentials the sandbox env already carried must survive the overlay.
	if len(env) <= 2 {
		t.Errorf("the overlay replaced the sandbox environment instead of merging onto it: %v", env)
	}
}

// Killing the host-side `docker exec` client does not reach the process it
// exec'd, so a sandboxed pi session survived every non-graceful end — holding
// the model session and its credentials, and stacking across retries (the leak
// already observed live for claude_code). The pidfile wrapper was wired but
// nothing ever read it back.
func TestPiRPCSandboxedExecuteReapsTheInContainerProcess(t *testing.T) {
	rec := &recordingRun{script: "exit 0"}
	task := Task{
		NodeID: "leaky", Iteration: 1,
		WorkDir: t.TempDir(), Sandbox: rec,
		Model: "mock/scripted",
	}

	b := &PiRPCBackend{Command: "pi", Logger: testLogger()}
	// The handshake cannot succeed against `exit 0`; the reaper must still run.
	_, _ = b.Execute(context.Background(), task)

	// The mark carries a random component, so the assertion cannot recompute
	// it — which is exactly why Execute has to compute it ONCE and hand the
	// same value to the spawner. Match the pidfile the spawn actually wrote
	// against the pidfile the reap actually referenced.
	var spawned, reaped string
	for _, argv := range rec.allArgv {
		joined := strings.Join(argv, " ")
		i := strings.Index(joined, "/tmp/iterion-delegate-")
		if i < 0 {
			continue
		}
		ref := strings.Fields(joined[i:])[0]
		ref = strings.TrimRight(ref, ";\"'")
		// The spawn wrapper also clears a stale pid, so "kill" alone does not
		// tell them apart: the spawn ends in `exec "$@"`, the reap sends TERM.
		switch {
		case strings.Contains(joined, `exec "$@"`):
			spawned = ref
		case strings.Contains(joined, "kill -TERM"):
			reaped = ref
		}
	}
	if spawned == "" {
		t.Fatalf("the sandboxed spawn never wrote a pidfile: %v", rec.allArgv)
	}
	if reaped == "" {
		t.Fatalf("no reap command was issued — the in-container pi would survive")
	}
	if reaped != spawned {
		t.Errorf("reaped %q but the spawn wrote %q — the reaper targets a different process", reaped, spawned)
	}
}

// The no-progress watchdog measures from lastProgress, which ONLY completion
// events write; sawFirstEvent arms off lastEvent, which every event bumps.
// Unguarded, the first tick after a mere agent_start measured against a zero
// timestamp — ~2000 years — and aborted the turn as "no progress".
func TestPiCollectorNoProgressIsNotArmedByAZeroMark(t *testing.T) {
	c := &piCollector{task: Task{NodeID: "n"}, logger: testLogger(), settled: make(chan struct{})}

	// A non-completion event: it must arm sawFirstEvent WITHOUT claiming
	// progress, which is exactly the state the watchdog mishandled.
	c.onEvent(pisdk.Event{Type: pisdk.EventAgentStart})

	last, lastProgress := c.marks()
	if last.IsZero() {
		t.Fatal("a non-completion event must still count as stream activity")
	}
	if !lastProgress.IsZero() {
		t.Fatal("a non-completion event must not count as progress — the test's premise is gone")
	}
	// The guard the fix adds: with a zero progress mark, elapsed time is
	// meaningless and must not trip the watchdog.
	if !lastProgress.IsZero() && time.Since(lastProgress) > piNoProgressTimeout {
		t.Fatal("unreachable")
	}
	if time.Since(lastProgress) <= piNoProgressTimeout {
		t.Fatalf("a zero mark should measure as ancient (%v) — the hazard would not exist otherwise",
			time.Since(lastProgress))
	}
}
