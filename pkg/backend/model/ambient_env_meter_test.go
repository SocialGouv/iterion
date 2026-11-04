package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/internal/credheader"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// blankAmbientEnv states a pod with no credential in any channel claw or the
// claude CLI reads, no switch, and a Claude Code config dir holding no login:
// claw's factories read the host's login last, so a developer machine would
// otherwise decide these rows. A row sets what it needs on top.
func blankAmbientEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_OAUTH_TOKEN", "ZAI_API_KEY", "ITERION_FORBID_SUBSCRIPTION_OAUTH",
		"CLAW_USER_AGENT", "ITERION_LLM_USER_AGENT",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// resolveUnderHint resolves spec the way a claw node or a supervisor does:
// through the registry's context-aware path, under the provider hint it
// carries.
func resolveUnderHint(spec, hint string) (api.APIClient, error) {
	return NewRegistry().ResolveWithContext(WithProviderHint(context.Background(), hint), spec)
}

// wireCredentials points the env factory at a stub gateway, resolves spec
// through the real registry, sends one request and returns the credential
// headers it carried — the artefact the meter's reading must name.
func wireCredentials(t *testing.T, spec string) map[string]string {
	t.Helper()
	got := make(chan map[string]string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- map[string]string{"x-api-key": r.Header.Get("x-api-key"), "authorization": r.Header.Get("Authorization")}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("ANTHROPIC_BASE_URL", ts.URL)
	client, err := resolveUnderHint(spec, "")
	if err != nil {
		t.Fatalf("bench: the factory refused %s: %v", spec, err)
	}
	_, id, _ := ParseModelSpec(spec)
	ch, err := client.StreamResponse(context.Background(), api.CreateMessageRequest{
		Model:     id,
		MaxTokens: 16,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("bench: the client sent nothing: %v", err)
	}
	for range ch {
	}
	select {
	case sent := <-got:
		return sent
	case <-time.After(5 * time.Second):
		t.Fatal("bench: the stub gateway never received the request")
		return nil
	}
}

// liveFromWire names what a request carried by the env channel each value
// came from — a token variable, or a credential line of the custom-header
// block under the block's name and its own — so the expected fingerprint is
// read off the wire rather than off a second description of the factory's
// order.
func liveFromWire(t *testing.T, sent map[string]string) map[string]string {
	t.Helper()
	headers := os.Getenv("ANTHROPIC_CUSTOM_HEADERS")
	live := map[string]string{}
	if k := sent["x-api-key"]; k != "" {
		line, _ := credheader.LineValue(headers, credheader.NameXAPIKey)
		switch {
		case line != "" && k == line:
			live["ANTHROPIC_CUSTOM_HEADERS:x-api-key"] = k
		case k == os.Getenv("ANTHROPIC_API_KEY"):
			live["ANTHROPIC_API_KEY"] = k
		default:
			t.Fatalf("bench: the request carried an x-api-key no channel of the row holds")
		}
	}
	if a := sent["authorization"]; a != "" {
		line, _ := credheader.LineValue(headers, credheader.NameAuthorization)
		switch {
		case line != "" && a == line:
			live["ANTHROPIC_CUSTOM_HEADERS:authorization"] = a
		case os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" && a == "Bearer "+os.Getenv("ANTHROPIC_AUTH_TOKEN"):
			live["ANTHROPIC_AUTH_TOKEN"] = os.Getenv("ANTHROPIC_AUTH_TOKEN")
		default:
			t.Fatalf("bench: the request carried an Authorization header no channel of the row holds")
		}
	}
	return live
}

// The meter names the account claw's client actually sends, read off the
// wire: claw reads no CLAUDE_CODE_OAUTH_TOKEN, sends a key alone where a
// bearer sits beside it, and sends no line of the ambient custom-header block
// — the engine's client refuses it (withClientIdentity, NoAmbientHeaders),
// where the claude CLI applies it (an Authorization line replaces the bearer,
// an x-api-key line the key, an empty one erases what it replaces). The
// claude CLI's reading of the same pod (delegate.AmbientCredentialFingerprint)
// names another account on the claudeDiffers rows, which is what keyed claw's
// walls on a forfait it never spent — and the SAME account on every other
// row: where both lanes send one set they key one meter, or claw's routes
// never meet the walls the claude_code sessions record.
func TestAmbientEnvMeter_NamesTheCredentialClawSends(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		spec string
		// claudeDiffers: the claude CLI's reading of this pod names another
		// account — the row would pass under the wrong reading otherwise.
		claudeDiffers bool
	}{
		{"a key beside the forfait token", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-forfait"}, "anthropic/claude-sonnet-4", true},
		{"a key beside a bearer", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "ANTHROPIC_AUTH_TOKEN": "tok-pod"}, "anthropic/claude-sonnet-4", true},
		{"a bearer beside the forfait token", map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok-pod", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-forfait"}, "anthropic/claude-sonnet-4", true},
		{"a key and a header credential", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "ANTHROPIC_CUSTOM_HEADERS": "X-Tenant: a\nAuthorization: Bearer hdr-pod"}, "anthropic/claude-sonnet-4", true},
		{"a header credential over a bearer", map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok-pod", "ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer hdr-pod"}, "anthropic/claude-sonnet-4", true},
		{"an x-api-key line over the key", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "ANTHROPIC_CUSTOM_HEADERS": "x-api-key: hdr-key"}, "anthropic/claude-sonnet-4", true},
		{"an x-api-key line beside a bearer", map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok-pod", "ANTHROPIC_CUSTOM_HEADERS": "x-api-key: hdr-key"}, "anthropic/claude-sonnet-4", true},
		{"an empty Authorization line over a bearer", map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok-pod", "ANTHROPIC_CUSTOM_HEADERS": "Authorization:"}, "anthropic/claude-sonnet-4", true},
		{"a GLM id on a keyed gateway", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-forfait"}, "anthropic/glm-5.3", true},
		{"the forfait token alone", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-forfait"}, "anthropic/claude-sonnet-4", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankAmbientEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			live := liveFromWire(t, wireCredentials(t, tc.spec))
			want := delegate.AmbientCredentialSetFingerprint(live)
			fp, metered := AmbientEnvMeter(tc.spec, "")
			if !metered {
				t.Fatalf("AmbientEnvMeter(%s) metered nothing — the client sent %d credential(s)", tc.spec, len(live))
			}
			if fp != want {
				t.Errorf("AmbientEnvMeter(%s) = %q, want %q — the account the request carried", tc.spec, fp, want)
			}
			claude := delegate.AmbientCredentialFingerprint()
			if tc.claudeDiffers && claude == want {
				t.Fatalf("inert bench: the claude CLI's reading names the same account here")
			}
			if !tc.claudeDiffers && claude != want {
				t.Errorf("the claude CLI's reading of this pod = %q, claw's = %q — both send the same set, so one account sits on two meters", claude, want)
			}
		})
	}

	// No header line rides a claw request in process (the engine's client
	// refuses the ambient block) nor in the container (the crossing strips
	// its credential lines): a header-bearing pod keys the key's own account
	// on both sides, the account the wire carries above.
	t.Run("a header-bearing pod keys the key's own account", func(t *testing.T) {
		blankAmbientEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
		t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer hdr-pod")
		fp, metered := AmbientEnvMeter("anthropic/claude-sonnet-4", "")
		if want := delegate.AmbientCredentialSetFingerprint(map[string]string{"ANTHROPIC_API_KEY": "sk-pod"}); !metered || fp != want {
			t.Errorf("AmbientEnvMeter = %q metered=%v, want the key's own account %q", fp, metered, want)
		}
		if fp == delegate.AmbientCredentialFingerprint() {
			t.Error("inert bench: the claude CLI's reading of this pod names the same account")
		}
	})
}

// Where no env credential rides the request, claw's reading says so — the
// no-credential meter's spend — rather than the account the claude CLI would
// spend on the same pod: the forfait token alone, this host's login on disk,
// nothing at all.
func TestAmbientEnvMeter_NoEnvCredentialIsTheBareSpend(t *testing.T) {
	t.Run("the forfait token alone", func(t *testing.T) {
		blankAmbientEnv(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-forfait")
		if delegate.AmbientCredentialFingerprint() == "" {
			t.Fatal("inert bench: the claude CLI's reading names no account on this pod")
		}
		if fp, metered := AmbientEnvMeter("anthropic/claude-sonnet-4", ""); !metered || fp != "" {
			t.Errorf("AmbientEnvMeter = %q metered=%v, want the bare spend — claw reads no CLAUDE_CODE_OAUTH_TOKEN", fp, metered)
		}
		client, err := resolveUnderHint("anthropic/claude-sonnet-4", "")
		if cc, ok := client.(*api.Client); err != nil || !ok || cc.APIKey != "" || cc.OAuthToken != "" {
			t.Fatalf("bench: the factory built %T (err=%v) carrying a credential", client, err)
		}
	})
	t.Run("this host's login", func(t *testing.T) {
		blankAmbientEnv(t)
		t.Setenv("CLAUDE_CONFIG_DIR", writeClaudeCodeCreds(t))
		if fp, metered := AmbientEnvMeter("anthropic/claude-sonnet-4", ""); !metered || fp != "" {
			t.Errorf("AmbientEnvMeter = %q metered=%v, want the bare spend — no variable names a disk login", fp, metered)
		}
		client, err := resolveUnderHint("anthropic/claude-sonnet-4", "")
		if err != nil || clientBearerForTest(client) != "sk-ant-oat-test" {
			t.Fatalf("bench: the factory did not spend the host's login (err=%v)", err)
		}
	})
}

// Where claw's walk refuses before any client exists, nothing spends: the
// meter reads no wall, and the typed refusal is the call's answer. Every row
// is judged against the real resolution, never a second description of it.
func TestAmbientEnvMeter_NothingSpendsWhereTheWalkRefuses(t *testing.T) {
	const gateway = "https://llm-gw.example.test/anthropic"
	var (
		forbidden   = func(err error) bool { return errors.Is(err, secrets.ErrSubscriptionOAuthForbidden) }
		unavailable = func(err error) bool {
			var e *delegate.ErrModelUnavailable
			return errors.As(err, &e)
		}
		unfunded = func(err error) bool {
			var e *ErrProviderHintUnfunded
			return errors.As(err, &e)
		}
	)
	for _, tc := range []struct {
		name    string
		env     map[string]string
		forfait bool
		spec    string
		hint    string
		refusal func(error) bool
	}{
		{"a subscription bearer under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_AUTH_TOKEN": "sk-ant-oat-pod"}, false,
			"anthropic/claude-sonnet-4", "", forbidden},
		{"a gateway bearer under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_AUTH_TOKEN": "tok-gw"}, false,
			"anthropic/claude-sonnet-4", "", forbidden},
		{"a GLM id on the gateway under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_AUTH_TOKEN": "tok-gw"}, false,
			"anthropic/glm-5.3", "", forbidden},
		{"the host's login under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1"}, true,
			"anthropic/claude-sonnet-4", "", forbidden},
		{"a bare id under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_AUTH_TOKEN": "sk-ant-oat-pod"}, false,
			"claude-sonnet-4", "", forbidden},
		{"the ZAI_API_KEY synthesis refusing an id z.ai does not serve",
			map[string]string{"ZAI_API_KEY": "zai-pod", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-forfait"}, false,
			"anthropic/claude-sonnet-4", "", unavailable},
		{"the direct path with no key and no login",
			map[string]string{"ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_AUTH_TOKEN": "tok-gw", "ZAI_API_KEY": "zai-pod"}, false,
			"anthropic/claude-sonnet-4", "anthropic", unfunded},
		{"the direct path's login under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1"}, true,
			"anthropic/claude-sonnet-4", "anthropic", forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankAmbientEnv(t)
			if tc.forfait {
				t.Setenv("CLAUDE_CONFIG_DIR", writeClaudeCodeCreds(t))
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			spec := tc.spec
			if !strings.Contains(spec, "/") {
				spec = "anthropic/" + spec
			}
			if _, err := resolveUnderHint(spec, tc.hint); !tc.refusal(err) {
				t.Fatalf("bench: the resolution answered err=%v, not the refusal the row pins", err)
			}
			if fp, metered := AmbientEnvMeter(tc.spec, tc.hint); metered {
				t.Errorf("AmbientEnvMeter(%s, hint %q) = %q metered — the walk refuses, no account spends", tc.spec, tc.hint, fp)
			}
		})
	}

	// A key ahead of the bearer is spent, not refused.
	t.Run("an API key ahead of the bearer under the switch", func(t *testing.T) {
		blankAmbientEnv(t)
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat-pod")
		if _, err := resolveUnderHint("anthropic/claude-sonnet-4", ""); err != nil {
			t.Fatalf("bench: the factory refused (err=%v)", err)
		}
		if fp, metered := AmbientEnvMeter("anthropic/claude-sonnet-4", ""); !metered || fp != secrets.FingerprintSHA256("sk-pod") {
			t.Errorf("AmbientEnvMeter = %q metered=%v, want the key's account", fp, metered)
		}
	})
}

// The `provider: "anthropic"` hint takes claw down the direct path: the
// ambient key, else this host's login, both to api.anthropic.com — the
// bearer, the base URL and the z.ai synthesis unread. The meter follows it:
// on a gateway pod carrying a bearer, a hinted call spends the key, or the
// host's login (the no-credential meter's spend), never the bearer's account
// the unhinted env path spends.
func TestAmbientEnvMeter_TheDirectHintReadsTheDirectPath(t *testing.T) {
	const spec = "anthropic/claude-sonnet-4"
	blankAmbientEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-gw")
	t.Setenv("CLAUDE_CONFIG_DIR", writeClaudeCodeCreds(t))
	if fp, metered := AmbientEnvMeter(spec, ""); !metered || fp != secrets.FingerprintSHA256("tok-gw") {
		t.Fatalf("bench: the unhinted env path = %q metered=%v, want the gateway bearer's account", fp, metered)
	}

	client, err := resolveUnderHint(spec, "anthropic")
	if err != nil || clientBearerForTest(client) != "sk-ant-oat-test" {
		t.Fatalf("bench: the direct path did not spend the host's login (err=%v)", err)
	}
	if fp, metered := AmbientEnvMeter(spec, "anthropic"); !metered || fp != "" {
		t.Errorf("AmbientEnvMeter under the hint = %q metered=%v, want the bare spend of the host's login — the hint never sends the bearer", fp, metered)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	client, err = resolveUnderHint(spec, "anthropic")
	if cc, ok := client.(*api.Client); err != nil || !ok || cc.APIKey != "sk-pod" || cc.OAuthToken != "" {
		t.Fatalf("bench: the direct path built %T (err=%v), not a key-only client", client, err)
	}
	if fp, metered := AmbientEnvMeter(spec, "anthropic"); !metered || fp != secrets.FingerprintSHA256("sk-pod") {
		t.Errorf("AmbientEnvMeter under the hint = %q metered=%v, want the key's account", fp, metered)
	}
}

// A GLM id is served by z.ai, never by api.anthropic.com. With nothing held
// the meter reads no wall where the env walk sends the id somewhere no
// account of this cap can stand in its way: z.ai's own account through the
// synthesis, api.anthropic.com — an empty base URL or that host named
// outright — or a request carrying no credential at all.
func TestAmbientEnvMeter_AGLMIDBoundWhereNoMeteredAccountServesIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		wantBase string
	}{
		{"the z.ai synthesis", map[string]string{"ZAI_API_KEY": "zai-pod", "ANTHROPIC_API_KEY": "sk-pod"}, secrets.ZAIDefaultBaseURL},
		// The provider's own default, which the client resolves an empty base
		// URL to.
		{"api.anthropic.com by default", map[string]string{"ANTHROPIC_API_KEY": "sk-pod"}, "https://api.anthropic.com"},
		{"api.anthropic.com named outright", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "ANTHROPIC_BASE_URL": "https://api.anthropic.com"}, "https://api.anthropic.com"},
		{"a bearer dropped on z.ai's own host", map[string]string{"ANTHROPIC_AUTH_TOKEN": "zai-byok", "ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"}, "https://api.z.ai/api/anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankAmbientEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			client, err := resolveUnderHint("anthropic/glm-5.3", "")
			cc, ok := client.(*api.Client)
			if err != nil || !ok {
				t.Fatalf("bench: the factory built %T (err=%v)", client, err)
			}
			if cc.BaseURL != tc.wantBase {
				t.Fatalf("bench: the client calls %q, not the destination the row pins", cc.BaseURL)
			}
			if fp, metered := AmbientEnvMeter("anthropic/glm-5.3", ""); metered {
				t.Errorf("AmbientEnvMeter(anthropic/glm-5.3) = %q metered — no account of this cap serves the id there", fp)
			}
		})
	}
}
