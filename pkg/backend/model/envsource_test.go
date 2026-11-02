package model

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The seam's reason to exist (#2808): tests that run in parallel cannot
// t.Setenv (the testing package panics), so each carries its own
// environment on the context and reads its own values back. Mutations
// this witnesses against: the seam ignoring the carried environment (both
// subtests read the process instead — red) and a SHARED environment (each
// subtest reads the other's value — red). The hermeticity against the
// PROCESS environment is the serial sibling file's witness
// (envsource_process_test.go — seeding the process is global state, the
// parallel campaign's table owns it).
func TestEnvSeamParallelTestsReadTheirOwnValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value, other string }{
		{"a", "value-a", "b"},
		{"b", "value-b", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := withCarriedEnv(context.Background(), carriedEnv{
				"ITERION_ENVSEAM_SHARED_NAME":                      tc.value,
				"ITERION_ENVSEAM_ONLY_" + strings.ToUpper(tc.name): "mine-" + tc.name,
			})
			if got := envGetenv(ctx, "ITERION_ENVSEAM_SHARED_NAME"); got != tc.value {
				t.Errorf("the same name, two parallel values: read %q, want my own %q", got, tc.value)
			}
			if got := envGetenv(ctx, "ITERION_ENVSEAM_ONLY_"+strings.ToUpper(tc.other)); got != "" {
				t.Errorf("a name only the SIBLING carries read %q through my environment: the seam leaked across tests", got)
			}
			if _, ok := envLookupEnv(ctx, "ITERION_ENVSEAM_NEVER_SET"); ok {
				t.Error("LookupEnv of a name neither side holds: planted=true")
			}
			if v, ok := envLookupEnv(ctx, "ITERION_ENVSEAM_SHARED_NAME"); !ok || v != tc.value {
				t.Errorf("envLookupEnv = %q,%v, want %q,true (planted reads through)", v, ok, tc.value)
			}
		})
	}
}

// The whole-env shape: sorted k=v pairs, and ONLY what the map holds.
func TestCarriedEnvEnviron(t *testing.T) {
	t.Parallel()
	ctx := withCarriedEnv(context.Background(), carriedEnv{"B": "2", "A": "1"})
	got := envEnviron(ctx)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Fatalf("envEnviron = %v, want [A=1 B=2] (sorted, hermetic)", got)
	}
	if got := envEnviron(context.Background()); len(got) == 0 {
		t.Fatal("nothing carried: envEnviron must answer the process environment")
	}
	// An empty non-nil map is a deliberately empty environment — not the
	// process default.
	empty := withCarriedEnv(context.Background(), carriedEnv{})
	if got := envEnviron(empty); len(got) != 0 {
		t.Fatalf("empty carried env: envEnviron = %v, want empty", got)
	}
	// A name nothing seeded anywhere (unique per run) reads unset through
	// the empty environment — the process is not consulted.
	never := fmt.Sprintf("ITERION_ENVSEAM_EMPTY_%d", os.Getpid())
	if got := envGetenv(empty, never); got != "" {
		t.Errorf("empty carried env: envGetenv(%s) = %q, want unset", never, got)
	}
}

// The security half of the seam (#2808): secretguard's ambient-secret
// enumeration (the whole-env reader) and its kill-switch read through
// it. A secret whose env name is seen through the carried environment is
// STILL redacted; the mutant — the seam's Environ answering empty —
// turns the redaction assertion red, not a compile error.
func TestBuildSecretGuardRedactsThroughTheCarriedEnv(t *testing.T) {
	t.Parallel()
	const value = "sk-acme-9f8e7d6c5b4a39281706f5e4d3c2b1a0987654321fedcba"
	ctx := withCarriedEnv(context.Background(), carriedEnv{
		"ACME_ENVSEAM_API_KEY": value,
	})
	guard := BuildSecretGuard(ctx, &ir.Workflow{}, nil, nil)
	if guard == nil {
		t.Fatal("redaction enabled through the seam: BuildSecretGuard returned nil (ITERION_SECRETS_REDACT=off would do that — is the seam leaking one?)")
	}
	redacted := guard.Redact("the key is " + value + " ok")
	if strings.Contains(redacted, value) {
		t.Fatalf("the ambient secret read through the seam was NOT redacted: %q — the seam answering empty here is a secret in a log", redacted)
	}

	// The kill-switch through the seam too: ITERION_SECRETS_REDACT=off
	// disables SINK redaction (the guard stays live for materialisation —
	// Layer 1's mirror never follows the sink switch), so Redact leaves
	// the carried value untouched. Proves the named reads take the seam
	// as well as the whole-env one.
	ctxOff := withCarriedEnv(context.Background(), carriedEnv{
		"ACME_ENVSEAM_API_KEY":   value,
		"ITERION_SECRETS_REDACT": "off",
	})
	if guardOff := BuildSecretGuard(ctxOff, &ir.Workflow{}, nil, nil); guardOff != nil {
		if out := guardOff.Redact("the key is " + value + " ok"); !strings.Contains(out, value) {
			t.Fatalf("ITERION_SECRETS_REDACT=off through the seam must leave the value in the sink, got %q — the named reads bypass the seam", out)
		}
	}
}

// The slice-2 migration witnesses (#2808): every function below reads its
// env through the seam, so two parallel subtests carrying DIFFERENT values
// of the same name each read their own back — the ticket's per-name
// isolation scenario on the migrated readers themselves. The mutant each
// witnesses against is the migration undone — the site reading the process
// (os.Getenv) instead of the carried environment: at least one side of
// each pair reddens (the hermetic side always does — the process holds
// nothing the carried map does not name). The process-default half of
// these readers stays pinned by the serial family's t.Setenv tests.
func TestMigratedReadersReadTheirOwnCarriedEnv(t *testing.T) {
	t.Parallel()

	t.Run("anthropicFunding", func(t *testing.T) {
		t.Parallel()
		funded := anthropicFunding(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-ant-carried"}))
		if funded != "the environment's anthropic credential" {
			t.Errorf("carried ANTHROPIC_API_KEY: anthropicFunding = %q, want the env credential", funded)
		}
		// The hermetic twin: a carried env naming only a z.ai wire holds no
		// anthropic credential — the process's own keys must not answer.
		if got := anthropicFunding(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"})); got != "" {
			t.Errorf("carried env without a credential: anthropicFunding = %q, want unfunded (the process leaked)", got)
		}
	})

	t.Run("zaiKeyReachable", func(t *testing.T) {
		t.Parallel()
		if !zaiKeyReachable(withCarriedEnv(context.Background(), carriedEnv{"ZAI_API_KEY": "zai-carried"})) {
			t.Error("carried ZAI_API_KEY: zaiKeyReachable = false")
		}
		if zaiKeyReachable(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-ant-carried"})) {
			t.Error("carried env without ZAI_API_KEY: zaiKeyReachable = true (the process leaked)")
		}
	})

	t.Run("glmKeyReachableUnderHint", func(t *testing.T) {
		t.Parallel()
		if !glmKeyReachableUnderHint(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_BASE_URL": "https://gw.example"})) {
			t.Error("carried ANTHROPIC_BASE_URL: glmKeyReachableUnderHint = false")
		}
		if glmKeyReachableUnderHint(withCarriedEnv(context.Background(), carriedEnv{"OPENAI_API_KEY": "sk-other"})) {
			t.Error("carried env without a z.ai route: glmKeyReachableUnderHint = true (the process leaked)")
		}
	})

	t.Run("forwardableProviderEnv", func(t *testing.T) {
		t.Parallel()
		env, _, err := forwardableProviderEnv(withCarriedEnv(context.Background(), carriedEnv{"OPENAI_API_KEY": "sk-carried"}), "anthropic/claude-haiku-4-5", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := env["OPENAI_API_KEY"]; got != "sk-carried" {
			t.Errorf("the crossing carries %q, want the carried value", got)
		}
		env, _, err = forwardableProviderEnv(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-ant-other"}), "anthropic/claude-haiku-4-5", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := env["OPENAI_API_KEY"]; got != "" {
			t.Errorf("a carried env naming no OPENAI_API_KEY forwards %q: the process leaked into the crossing", got)
		}
	})

	t.Run("forwardableToolEnv", func(t *testing.T) {
		t.Parallel()
		env := forwardableToolEnv(withCarriedEnv(context.Background(), carriedEnv{"CLAW_BASH_TIMEOUT": "30s"}))
		if got := env["CLAW_BASH_TIMEOUT"]; got != "30s" {
			t.Errorf("carried CLAW_BASH_TIMEOUT: forwarded %q", got)
		}
		if got := forwardableToolEnv(withCarriedEnv(context.Background(), carriedEnv{"CLAW_BASH_TIMEOUT": "30s"}))["CLAW_CONFIG_HOME"]; got != "" {
			t.Errorf("a carried env naming no CLAW_CONFIG_HOME forwards %q: the process leaked", got)
		}
	})

	t.Run("openAIOAuthAllowed", func(t *testing.T) {
		t.Parallel()
		if openAIOAuthAllowed(withCarriedEnv(context.Background(), carriedEnv{"ITERION_OPENAI_USE_OAUTH": "0"})) {
			t.Error("carried ITERION_OPENAI_USE_OAUTH=0: allowed = true")
		}
		if !openAIOAuthAllowed(withCarriedEnv(context.Background(), carriedEnv{"ITERION_OPENAI_USE_OAUTH": "1"})) {
			t.Error("carried ITERION_OPENAI_USE_OAUTH=1 and no OPENAI_BASE_URL: allowed = false (the process's OPENAI_BASE_URL leaked)")
		}
	})

	t.Run("ambientAnthropicCredential", func(t *testing.T) {
		t.Parallel()
		if !ambientAnthropicCredential(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_AUTH_TOKEN": "oat-carried"})) {
			t.Error("carried ANTHROPIC_AUTH_TOKEN: false")
		}
		if ambientAnthropicCredential(withCarriedEnv(context.Background(), carriedEnv{"OPENAI_API_KEY": "sk-other"})) {
			t.Error("carried env without an anthropic credential: true (the process leaked)")
		}
	})

	t.Run("workspaceAgentMaxBytes", func(t *testing.T) {
		t.Parallel()
		if got := workspaceAgentMaxBytes(withCarriedEnv(context.Background(), carriedEnv{"ITERION_CLAW_WORKSPACE_AGENT_MAX_BYTES": "4096"})); got != 4096 {
			t.Errorf("carried ceiling: %d, want 4096", got)
		}
		if got := workspaceAgentMaxBytes(withCarriedEnv(context.Background(), carriedEnv{"CLAW_BASH_TIMEOUT": "30s"})); got != defaultWorkspaceAgentMaxBytes {
			t.Errorf("a carried env naming no ceiling: %d, want the default %d (the process leaked)", got, defaultWorkspaceAgentMaxBytes)
		}
	})

	t.Run("openAIForfaitConfig", func(t *testing.T) {
		t.Parallel()
		cfg := openAIForfaitConfig(withCarriedEnv(context.Background(), carriedEnv{"OPENAI_BASE_URL": "https://gw.example"}), "gpt-6", secrets.CodexCredentialsView{})
		if cfg.BaseURL != "https://gw.example" {
			t.Errorf("carried OPENAI_BASE_URL: BaseURL = %q", cfg.BaseURL)
		}
		cfg = openAIForfaitConfig(withCarriedEnv(context.Background(), carriedEnv{"ITERION_OPENAI_USE_OAUTH": "1"}), "gpt-6", secrets.CodexCredentialsView{})
		if cfg.BaseURL != "" {
			t.Errorf("a carried env naming no OPENAI_BASE_URL: BaseURL = %q (the process leaked)", cfg.BaseURL)
		}
	})
}

// The slice-3 witnesses (#2808): the three remaining whole-env readers —
// the two tool-node command builders and the settings command hook — run
// their child on the environment the CONTEXT carries. The mutant each
// witnesses against is os.Environ() back at the site: the carried name
// vanishes from the child's env (red), and the process's own PATH — which
// no carried map here names — reappears (red the other way).
func TestToolNodeCommandsRunOnTheCarriedEnv(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{}
	ctx := withCarriedEnv(context.Background(), carriedEnv{"ITERION_ENVSEAM_TOOLCMD": "carried"})

	cmd := e.toolNodeCommand(ctx, "true", nil).cmd
	if got := envValue(cmd.Env, "ITERION_ENVSEAM_TOOLCMD"); got != "carried" {
		t.Errorf("toolNodeCommand env ITERION_ENVSEAM_TOOLCMD = %q, want the carried value", got)
	}
	sc := e.toolNodeScriptCommand(ctx, []string{"sh"}, "s.sh", nil).cmd
	if got := envValue(sc.Env, "ITERION_ENVSEAM_TOOLCMD"); got != "carried" {
		t.Errorf("toolNodeScriptCommand env ITERION_ENVSEAM_TOOLCMD = %q, want the carried value", got)
	}
	if os.Getenv("PATH") != "" {
		// Hermetic: the process's own PATH is not the carried env's, so it
		// must not reach the child (the os.Environ mutant reddens here).
		if got := envValue(cmd.Env, "PATH"); got != "" {
			t.Errorf("toolNodeCommand env PATH = %q: the process environment leaked into the child", got)
		}
		if got := envValue(sc.Env, "PATH"); got != "" {
			t.Errorf("toolNodeScriptCommand env PATH = %q: the process environment leaked into the child", got)
		}
	}
}

func TestACommandHookRunsOnTheCarriedEnv(t *testing.T) {
	t.Parallel()
	logger := iterlog.New(iterlog.LevelWarn, io.Discard)
	hook := settingsHookEntry{Type: "command", Command: `[ "$ITERION_ENVSEAM_HOOK" = "yes" ] && exit 2; exit 0`}

	blocked, _ := runCommandHook(withCarriedEnv(context.Background(), carriedEnv{"ITERION_ENVSEAM_HOOK": "yes"}), "PostToolUse", hooks.Context{ToolName: "bash"}, hook, logger)
	if !blocked {
		t.Error("the hook saw no ITERION_ENVSEAM_HOOK=yes: the carried environment did not reach its command")
	}
	blocked, _ = runCommandHook(withCarriedEnv(context.Background(), carriedEnv{"ITERION_ENVSEAM_HOOK": "no"}), "PostToolUse", hooks.Context{ToolName: "bash"}, hook, logger)
	if blocked {
		t.Error("a carried env holding ITERION_ENVSEAM_HOOK=no blocked the hook: it read another environment's value")
	}
}

// The slice-4 witnesses (#2808): the reader a HANDOFF carries. compatgw's
// entry points take a func(string) string, so the seam reaches them as a
// closure bound at the call site (the scanner's closure idiom). Both
// directions: the carried environment answers, and the process's does not
// leak. The mutant each witnesses against is os.Getenv handed back at the
// site — one side reddens whatever the process holds. Only VALID tables
// are carried: compatgw memoises a parse refusal process-wide (the sticky
// operatorRefusal), while distinct valid tables are memoised by content
// and are parallel-safe.
func TestGatewayEnvChecksReadTheCarriedEnvThroughTheHandoff(t *testing.T) {
	t.Parallel()
	if err := checkGatewayEnv(withCarriedEnv(context.Background(), carriedEnv{
		"OPENAI_COMPATIBLE_BASE_URL": "https://gw.invalid",
		"OPENAI_COMPATIBLE_API_KEY":  "sk-carried",
	}), false); err != nil {
		t.Errorf("carried gateway pair: %v — the handoff did not read the carried environment", err)
	}
	if err := checkGatewayEnv(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-other"}), false); err == nil {
		t.Error("a carried env without the gateway pair: nil error — the process's gateway env leaked")
	}
}

func TestGatewayCompactionReadsTheCarriedCatalogThroughTheHandoff(t *testing.T) {
	t.Parallel()
	const id = "openai_compatible/gw/kimi72-seam-witness"
	table := `{"gw/kimi72-seam-witness":{"context_window":4096,"max_output_tokens":128}}`
	cfg := compactionConfig(withCarriedEnv(context.Background(), carriedEnv{"OPENAI_COMPATIBLE_MODELS": table}), id, 0.5, 0)
	if cfg.MaxEstimatedTokens != 2048 {
		t.Errorf("carried catalog: MaxEstimatedTokens = %d, want 2048 (window 4096 x ratio 0.5)", cfg.MaxEstimatedTokens)
	}
	cfg = compactionConfig(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-other"}), id, 0.5, 0)
	if want := clawrt.DefaultCompactionConfig().MaxEstimatedTokens; cfg.MaxEstimatedTokens != want {
		t.Errorf("a carried env without the table: MaxEstimatedTokens = %d, want the default %d — the process's table leaked", cfg.MaxEstimatedTokens, want)
	}
}
