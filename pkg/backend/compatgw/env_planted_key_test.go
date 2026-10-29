package compatgw

import (
	"errors"
	"strings"
	"testing"
)

// The gateway pair is the environment's own or it is refused. Both halves of
// the presentation are checked, and so is the WORDING: a refusal that does
// not name the variable and the file leaves the operator reading claw's
// "provide either OPENAI_API_KEY or a (OAuthToken + OpenAIChatGPTAccountID)
// pair sourced from Codex CLI auth.json", two channels that serve no gateway.
func TestFromEnv_APlantedHalfRefusesTheGatewayByName(t *testing.T) {
	const (
		repoBase    = "https://gw.attacker.example"
		operatorKey = "sk-operator-real-key"
	)
	get := mapGet(map[string]string{BaseURLEnv: repoBase, APIKeyEnv: operatorKey})

	// The control FIRST: inherited, the pair authenticates as it always did.
	// Without this the narrowing could have cost every deployment its
	// gateway and the cases below would still pass.
	inheritEnv(t)
	cfg, err := FromEnv(get)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.APIKey != operatorKey || cfg.BaseURL != repoBase {
		t.Fatalf("an inherited pair read %q/%q: the deployment lost its gateway", cfg.BaseURL, cfg.APIKey)
	}

	for _, planted := range []string{BaseURLEnv, APIKeyEnv} {
		t.Run(planted, func(t *testing.T) {
			// A `.env` fills a name the operator left unset; `.env` loading
			// stops at the first file found walking up, so a repository's
			// file shadows a deployment one higher in the tree. Planting the
			// ENDPOINT would present the operator's own key to a host that
			// repository chose; planting the KEY would bill its reviewer's
			// calls to the virtual key it chose.
			plantEnv(t, planted)
			cfg, err := FromEnv(get)
			if err == nil {
				t.Fatalf("a planted %s configured the gateway: base=%q key=%q", planted, cfg.BaseURL, cfg.APIKey)
			}
			if cfg != (Config{}) {
				t.Errorf("the refusal came back with a config too: %+v", cfg)
			}
			for _, want := range []string{planted, "project `.env`", "export it in the environment the deployment runs with"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q, so the operator cannot act on it: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), operatorKey) {
				t.Error("the refusal printed the credential")
			}
		})
	}
}

// A gateway the environment names but authenticates with nothing is refused
// HERE, by the name the operator set. The degradation this replaces did not
// exist: claw's OpenAI provider — the one client this package builds —
// refuses an empty key unconditionally, so a Config carrying one bought a
// dead node whose message named neither this gateway nor the file that
// caused it. NewClient is driven in the same case, so the premise is read
// from the producer rather than asserted.
func TestFromEnv_AGatewayWithNoKeyIsRefusedByItsOwnName(t *testing.T) {
	inheritEnv(t)
	const base = "https://198.51.100.7"
	_, err := FromEnv(mapGet(map[string]string{BaseURLEnv: base}))
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("a keyless gateway = %v, want ErrNoKey", err)
	}
	if !strings.Contains(err.Error(), APIKeyEnv) {
		t.Errorf("the refusal does not name %s: %v", APIKeyEnv, err)
	}

	// The producer, on the config FromEnv refuses to return.
	if _, err := NewClient(Config{BaseURL: base}, "gw/m", false); err == nil {
		t.Fatal("claw's provider accepted an empty key: a keyless gateway builds a client after all, and ErrNoKey is now a limit of iterion's own making")
	}
	// And the control: the same endpoint with a key does build.
	if _, err := NewClient(Config{BaseURL: base, APIKey: "sk-real"}, "gw/m", false); err != nil {
		t.Fatalf("NewClient with a key: %v", err)
	}
}
