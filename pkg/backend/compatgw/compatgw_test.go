package compatgw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func TestFromEnv(t *testing.T) {
	if _, err := FromEnv(func(string) string { return "" }); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("an empty environment = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(ErrNotConfigured.Error(), BaseURLEnv) {
		t.Error("the refusal must name the variable to set")
	}
	cfg, err := FromEnv(func(name string) string {
		if name == BaseURLEnv {
			return "https://gw.example.com\n"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("a base URL with no key: %v", err)
	}
	if cfg.BaseURL != "https://gw.example.com" || cfg.APIKey != "" {
		t.Errorf("config = %+v, want the trimmed URL and no key", cfg)
	}
}

func TestValidate(t *testing.T) {
	// The accept side needs no DNS here: the strict refusals below all sit
	// on numeric addresses or reserved aliases (fail-closed, resolvable
	// offline), and the accept side of the dial guard is httpdial's own
	// suite. TestNewClient_RefusesARedirect exercises a full accepted
	// client (strict lifted, loopback — the escape hatch's own shape).
	for name, cfg := range map[string]Config{
		"no scheme":       {BaseURL: "gw.example.com"},
		"ftp":             {BaseURL: "ftp://gw.example.com"},
		"no host":         {BaseURL: "https:///v1"},
		"userinfo":        {BaseURL: "https://user:pass@gw.example.com"},
		"query":           {BaseURL: "https://gw.example.com/v1?key=x"},
		"fragment":        {BaseURL: "https://gw.example.com/v1#frag"},
		"loopback strict": {BaseURL: "http://127.0.0.1:4000"},
		"private strict":  {BaseURL: "http://10.0.0.9:4000"},
		"metadata strict": {BaseURL: "http://169.254.169.254"},
		"cluster alias":   {BaseURL: "http://gw.default.svc.cluster.local:4000"},
	} {
		if err := cfg.Validate(true); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// The escape hatch lifts the address requirement — for a self-hosted
	// gateway — and nothing else.
	for _, addr := range []string{"http://127.0.0.1:4000", "http://10.0.0.9:4000"} {
		if err := (Config{BaseURL: addr}).Validate(false); err != nil {
			t.Errorf("%s refused with the hatch lifted: %v", addr, err)
		}
	}
	if err := (Config{BaseURL: "https://user:pass@gw.example.com"}).Validate(false); err == nil {
		t.Error("embedded credentials survive the hatch")
	}
}

// A gateway that answers with a redirect is refused, typed and
// non-followed: the request stops at the policy, the credential never
// reaches the target, and the error carries the stable refusal.
func TestNewClient_RefusesARedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer gw.Close()

	cfg := Config{BaseURL: gw.URL, APIKey: "sk-test"}
	client, err := NewClient(cfg, "mymodel", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.StreamResponse(context.Background(), req())
	var refused *RedirectRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a RedirectRefusedError through the wrap", err)
	}
	if !strings.Contains(refused.Error(), BaseURLEnv) {
		t.Errorf("refusal %q does not say which variable to fix", refused.Error())
	}
}

// The transport trusts no ambient proxy and keeps the header floor.
func TestGuardedTransport(t *testing.T) {
	tr := guardedTransport(true)
	if tr.Proxy != nil {
		t.Error("an ambient proxy reached the gateway transport")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("no response-header floor with the watchdog off")
	}
}

// req is a minimal streaming chat request — the shape the provider sends
// the gateway before reading a byte of its answer.
func req() api.CreateMessageRequest {
	return api.CreateMessageRequest{
		Model:     "mymodel",
		MaxTokens: 16,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		Stream:    true,
	}
}

// NewClient performs NO DNS: it is the registry factory's constructor, and
// the registry caches its error for the process life — a transient resolver
// blip must never bake a poisoned client (or a poisoned error) into that
// cache. The full address validation with resolution lives in
// Config.Validate, at the dispatch seams. Red the moment someone "helpfully"
// calls Validate from NewClient again.
func TestNewClient_PerformsNoDNS(t *testing.T) {
	cfg := Config{BaseURL: "https://rva2-cannot-resolve.invalid", APIKey: "k"}
	if _, err := NewClient(cfg, "m", true); err != nil {
		t.Fatalf("NewClient on an unresolvable host: %v — the constructor resolved DNS", err)
	}
}
