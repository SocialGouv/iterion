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
	// A base URL with no key is ErrNoKey, not a keyless configuration: the
	// client builder refuses an empty key whatever this returns
	// (TestFromEnv_AGatewayWithNoKeyIsRefusedByItsOwnName drives it), so the
	// refusal belongs here, where it can still name the variable the
	// operator set.
	if _, err := FromEnv(mapGet(map[string]string{BaseURLEnv: "https://gw.example.com\n"})); !errors.Is(err, ErrNoKey) {
		t.Errorf("a base URL with no key = %v, want ErrNoKey", err)
	}
	cfg, err := FromEnv(mapGet(map[string]string{
		BaseURLEnv: "https://gw.example.com\n",
		APIKeyEnv:  " sk-real\n",
	}))
	if err != nil {
		t.Fatalf("the pair: %v", err)
	}
	if cfg.BaseURL != "https://gw.example.com" || cfg.APIKey != "sk-real" {
		t.Errorf("config = %+v, want both values trimmed", cfg)
	}
}

// Host is the host the client dials — what the run's proxy pins — and ""
// for a base URL no request reaches.
func TestConfigHost(t *testing.T) {
	for base, want := range map[string]string{
		"https://gw.example.com":           "gw.example.com",
		"https://GW.Example.com:8443/v1":   "GW.Example.com",
		"http://[2001:db8::1]:4000":        "2001:db8::1",
		"http://host.docker.internal:4000": "host.docker.internal",
		"https://gw.example.com/v1?x=1":    "",
		"ftp://gw.example.com":             "",
		"https://user:pw@gw.example.com":   "",
		"gw.example.com":                   "",
	} {
		if got := (Config{BaseURL: base}).Host(); got != want {
			t.Errorf("Config{BaseURL: %q}.Host() = %q, want %q", base, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	// The accept side needs no DNS here: the strict refusals below all sit
	// on numeric addresses, reserved aliases or malformed names, each
	// refused before any lookup, and the accept side of the dial guard is
	// httpdial's own suite. TestNewClient_RefusesARedirect exercises a full
	// accepted client (strict lifted, loopback — the escape hatch's own
	// shape).
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
		// The shared space and the non-public IPv6 transition addresses,
		// under the same hatch as the private ones (lifted below).
		"cgnat strict":                     {BaseURL: "http://100.64.0.1:4000"},
		"nat64 strict":                     {BaseURL: "http://[64:ff9b::7f00:1]:4000"},
		"nat64 local-use strict":           {BaseURL: "http://[64:ff9b:1::a00:1]:4000"},
		"6to4 strict":                      {BaseURL: "http://[2002:7f00:1::]:4000"},
		"dotted metadata alias strict":     {BaseURL: "http://metadata.google.internal./v1"},
		"dotted service alias strict":      {BaseURL: "http://gw.svc./v1"},
		"empty-label host strict":          {BaseURL: "http://gw..example.test/v1"},
		"double-dotted alias strict":       {BaseURL: "http://gw.svc../v1"},
		"metadata alias, cased, strict":    {BaseURL: "http://Metadata.Google.Internal/v1"},
		"kubernetes alias, dotted, strict": {BaseURL: "http://kubernetes.default./v1"},
	} {
		if err := cfg.Validate(true); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A reserved name is refused as the dial refuses it — by the guard,
	// before any lookup, in any spelling — so the refusal names the escape
	// hatch that lifts it; a resolver's failure would not.
	for _, base := range []string{"http://gw.default.svc.cluster.local:4000", "http://metadata.google.internal./v1", "http://gw.svc./v1", "http://kubernetes.default./v1"} {
		err := (Config{BaseURL: base}).Validate(true)
		if err == nil || !strings.Contains(err.Error(), "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1") {
			t.Errorf("Validate(true) on %s = %v; want the guard's refusal naming the escape hatch", base, err)
		}
	}
	// A malformed name is no address class: no hatch lifts it, and its
	// refusal names none.
	if err := (Config{BaseURL: "http://gw..example.test/v1"}).Validate(true); err == nil || strings.Contains(err.Error(), "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE") {
		t.Errorf("Validate(true) on an empty-label host = %v; want a refusal that names no escape hatch", err)
	}
	// An IPv4-compatible address is refused at every strictness, as the dial
	// refuses it: no hatch lifts it, so its refusal names none.
	for _, strict := range []bool{true, false} {
		if err := (Config{BaseURL: "http://[::127.0.0.1]:4000"}).Validate(strict); err == nil || strings.Contains(err.Error(), "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE") {
			t.Errorf("Validate(%v) on an IPv4-compatible loopback = %v; want a refusal that names no escape hatch", strict, err)
		}
	}
	// The escape hatch lifts the address requirement — for a self-hosted
	// gateway — and nothing else.
	for _, addr := range []string{"http://127.0.0.1:4000", "http://[::1]:4000", "http://10.0.0.9:4000", "http://100.64.0.1:4000", "http://[64:ff9b::7f00:1]:4000", "http://[64:ff9b:1::a00:1]:4000", "http://[2002:7f00:1::]:4000"} {
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
	tr, err := guardedTransport(true)
	if err != nil {
		t.Fatalf("guardedTransport with no engine proxy set: %v", err)
	}
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
