package model

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// openAIClientAuth reads the two credential fields of a resolved claw openai
// client. The client type lives in claw's internal package, so its exported
// fields are reached by name.
func openAIClientAuth(t *testing.T, client any) (apiKey, oauthToken string) {
	t.Helper()
	v := reflect.ValueOf(client)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		t.Fatalf("resolved client is a %T, not a struct pointer", client)
	}
	key, tok := v.FieldByName("APIKey"), v.FieldByName("OAuthToken")
	if !key.IsValid() || !tok.IsValid() {
		t.Fatalf("resolved client %T carries no APIKey/OAuthToken field", client)
	}
	return key.String(), tok.String()
}

// installRunLookups wires the lookups a cloud runner installs, so
// ResolveWithContext reads the run's credentials exactly as it does on a pod.
func installRunLookups(t *testing.T) {
	t.Helper()
	SetCredentialsLookup(RunCredentialsLookup)
	SetOAuthDirLookup(RunOAuthDirLookup)
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})
}

// claw spends, for an `openai/…` route: the run's default openai key, then its
// ChatGPT forfait (on plan), then a key a shared tier pinned for the route.
// The credential accounting leans on it through RegisterForfaitFirst: a pinned
// key served only by such routes, beside a sealed forfait, is never stamped.
// Spending the pinned key here while the accounting says it is idle would bill
// a metered key nobody counted.
func TestResolveWithContext_OpenAIForfaitBeforeAPinnedKey(t *testing.T) {
	cases := []struct {
		name      string
		defKey    string
		pinned    string
		forfait   bool
		oauthPref string
		wantKey   string
		wantToken string
	}{
		{name: "forfait before the key pinned for the route", pinned: "sk-pinned", forfait: true, wantToken: "tok-abc"},
		{name: "the run's own key before its forfait", defKey: "sk-default", pinned: "sk-pinned", forfait: true, wantKey: "sk-default"},
		{name: "no forfait: the pinned key serves the route", pinned: "sk-pinned", wantKey: "sk-pinned"},
		{name: "the kill switch keeps the forfait unspent", pinned: "sk-pinned", forfait: true, oauthPref: "0", wantKey: "sk-pinned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installRunLookups(t)
			// An empty CODEX_HOME: a fall-through to the disk factory must
			// find no forfait, never the host's own.
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("ITERION_OPENAI_USE_OAUTH", tc.oauthPref)
			creds := secrets.Credentials{
				APIKeys:       map[secrets.Provider]string{},
				PinnedAPIKeys: map[secrets.Provider]string{},
			}
			if tc.defKey != "" {
				creds.APIKeys[secrets.ProviderOpenAI] = tc.defKey
			}
			if tc.pinned != "" {
				creds.PinnedAPIKeys[secrets.ProviderOpenAI] = tc.pinned
			}
			if tc.forfait {
				creds.OAuthCredentialFiles = map[string]string{string(secrets.OAuthKindCodex): writeCodexAuth(t)}
			}
			client, err := NewRegistry().ResolveWithContext(secrets.WithCredentials(context.Background(), creds), "openai/gpt-6")
			if err != nil {
				t.Fatalf("ResolveWithContext: %v", err)
			}
			// Only the fixtures are printed: a wrong resolution may have read
			// a real credential.
			key, token := openAIClientAuth(t, client)
			if key != tc.wantKey {
				t.Errorf("client API key matches the fixture: false; want %q", tc.wantKey)
			}
			if token != tc.wantToken {
				t.Errorf("client OAuth token matches the fixture: false; want %q", tc.wantToken)
			}
		})
	}
}

// The sandbox twin: inside the container the runner rebuilds its registry from
// env, so the order is carried by ITERION_OPENAI_USE_OAUTH=1 beside the codex
// dir. It must follow the in-process order exactly — forced over a key pinned
// for this route, never over the run's own default key.
func TestForwardableProviderEnv_OpenAIForfaitBeforeAPinnedKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
	codex := map[string]string{string(secrets.OAuthKindCodex): "/host/tmp/codex"}

	pinnedOnly := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys:        map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-pinned"},
		OAuthCredentialFiles: codex,
	})
	env, err := forwardableProviderEnv(pinnedOnly, "openai/gpt-6")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if env["ITERION_OPENAI_USE_OAUTH"] != "1" {
		t.Error("ITERION_OPENAI_USE_OAUTH not forced beside a key pinned for this route — the container spends the pinned key while the in-process path spends the forfait")
	}
	if env["OPENAI_API_KEY"] != "sk-pinned" {
		t.Error("the pinned key did not cross — the route loses its fallback when the forfait blob is unreadable in the container")
	}

	withDefault := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-default"},
		PinnedAPIKeys:        map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-pinned"},
		OAuthCredentialFiles: codex,
	})
	env, err = forwardableProviderEnv(withDefault, "openai/gpt-6")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if _, forced := env["ITERION_OPENAI_USE_OAUTH"]; forced {
		t.Error("ITERION_OPENAI_USE_OAUTH forced over the run's own default key — the sandbox spends the forfait the in-process path leaves alone")
	}
	if env["OPENAI_API_KEY"] != "sk-default" {
		t.Error("the run's default openai key is not the one that crossed")
	}
}

// claw declares no forfait-first provider: on `openai/…` its forfait comes
// first only while the RUNNER lets it (openAIOAuthAllowed, a ChatGPT-mode
// blob), which the server's accounting cannot see — a declaration would leave
// a pinned key the runner spends unstamped. On `anthropic/…` the key comes
// first.
func TestClawDeclaresNoForfaitFirst(t *testing.T) {
	for _, p := range []secrets.Provider{secrets.ProviderOpenAI, secrets.ProviderAnthropic} {
		if delegate.ForfaitFirst(delegate.BackendClaw, string(p)) {
			t.Errorf("claw declared forfait-first for %s — its precedence there depends on the runner's env, or puts the key first", p)
		}
	}
}

// In process and in the sandbox, claw spends the SAME openai credential for
// every combination of the run's default key, a key pinned for the route, a
// ChatGPT forfait, the host's ITERION_OPENAI_USE_OAUTH (unset, the "0"
// refusal, a host-wide "1") and an OPENAI_BASE_URL — the container rebuilt
// from exactly what forwardableProviderEnv hands it, the seeded forfait in
// its CODEX_HOME.
func TestClawOpenAICredentialParity(t *testing.T) {
	type choice struct {
		failed     bool
		key, token string
	}
	pick := func(t *testing.T, client any, err error) choice {
		if err != nil {
			return choice{failed: true}
		}
		k, tok := openAIClientAuth(t, client)
		return choice{key: k, token: tok}
	}
	for _, defKey := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			for _, forfait := range []bool{false, true} {
				for _, useOAuth := range []string{"", "0", "1"} {
					for _, gateway := range []bool{false, true} {
						name := fmt.Sprintf("default=%v pinned=%v forfait=%v oauth=%q gateway=%v", defKey, pinned, forfait, useOAuth, gateway)
						t.Run(name, func(t *testing.T) {
							installRunLookups(t)
							blob := writeCodexAuth(t)
							baseURL := ""
							if gateway {
								baseURL = "http://gateway.parity.example/v1"
							}
							t.Setenv("OPENAI_API_KEY", "")
							t.Setenv("CODEX_HOME", t.TempDir())
							t.Setenv("ITERION_OPENAI_USE_OAUTH", useOAuth)
							t.Setenv("OPENAI_BASE_URL", baseURL)
							creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{}, PinnedAPIKeys: map[secrets.Provider]string{}}
							if defKey {
								creds.APIKeys[secrets.ProviderOpenAI] = "sk-default"
							}
							if pinned {
								creds.PinnedAPIKeys[secrets.ProviderOpenAI] = "sk-pinned"
							}
							if forfait {
								creds.OAuthCredentialFiles = map[string]string{string(secrets.OAuthKindCodex): blob}
							}
							ctx := secrets.WithCredentials(context.Background(), creds)
							hostClient, hostErr := NewRegistry().ResolveWithContext(ctx, "openai/gpt-6")
							host := pick(t, hostClient, hostErr)

							env, err := forwardableProviderEnv(ctx, "openai/gpt-6")
							if err != nil {
								t.Fatalf("forwardableProviderEnv: %v", err)
							}
							for _, name := range []string{"OPENAI_API_KEY", "ITERION_OPENAI_USE_OAUTH", "OPENAI_BASE_URL"} {
								t.Setenv(name, env[name])
							}
							if env["CODEX_HOME"] != "" {
								t.Setenv("CODEX_HOME", blob)
							}
							boxClient, boxErr := NewRegistry().Resolve("openai/gpt-6")
							box := pick(t, boxClient, boxErr)
							// Fixtures only in the messages: a wrong resolution may
							// have read a real credential.
							if host.failed != box.failed || host.key != box.key || host.token != box.token {
								t.Errorf("in process {failed=%v key=%q forfait=%v}, sandbox {failed=%v key=%q forfait=%v}",
									host.failed, fixtureOrOther(host.key), host.token != "", box.failed, fixtureOrOther(box.key), box.token != "")
							}
						})
					}
				}
			}
		}
	}
}

// The ITERION_OPENAI_USE_OAUTH=0 refusal crosses when the run carries no
// credentials too — a local run of a bot without a secrets: block: a ChatGPT
// forfait on the container's own disk (a devcontainer mount) is spent by
// neither side.
//
// Only the exact refusal crosses there: a host-wide "1" forces the host's own
// forfait, which crosses only in a run's credentials — it never orders the
// container to spend one its disk holds — and any other value refuses nothing,
// on either side.
func TestClawOpenAIRefusalParityWithoutRunCredentials(t *testing.T) {
	for _, useOAuth := range []string{"0", " 0", "1", ""} {
		t.Run(fmt.Sprintf("oauth=%q", useOAuth), func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("CODEX_HOME", writeCodexAuth(t))
			t.Setenv("ITERION_OPENAI_USE_OAUTH", useOAuth)
			hostClient, hostErr := NewRegistry().ResolveWithContext(context.Background(), "openai/gpt-6")

			env, err := forwardableProviderEnv(context.Background(), "openai/gpt-6")
			if err != nil {
				t.Fatalf("forwardableProviderEnv: %v", err)
			}
			want := ""
			if useOAuth == "0" {
				want = "0"
			}
			if got := env["ITERION_OPENAI_USE_OAUTH"]; got != want {
				t.Errorf("the host's ITERION_OPENAI_USE_OAUTH=%q crossed as %q without run credentials, want %q", useOAuth, got, want)
			}
			for _, name := range []string{"OPENAI_API_KEY", "ITERION_OPENAI_USE_OAUTH", "OPENAI_BASE_URL"} {
				t.Setenv(name, env[name])
			}
			boxClient, boxErr := NewRegistry().Resolve("openai/gpt-6")

			spent := func(client any, err error) bool {
				if err != nil {
					return false
				}
				_, token := openAIClientAuth(t, client)
				return token != ""
			}
			if host, box := spent(hostClient, hostErr), spent(boxClient, boxErr); host != box {
				t.Errorf("forfait spent in process = %v, in the sandbox = %v — the operator's setting did not cross as the host applies it", host, box)
			}
		})
	}
}

func fixtureOrOther(k string) string {
	switch k {
	case "", "sk-default", "sk-pinned":
		return k
	}
	return "<another key>"
}
