package errtrack

import (
	"fmt"
	"strings"
	"testing"

	sentry "github.com/getsentry/sentry-go"
)

// leakCheck fails the test when the scrubbed output still renders the
// secret anywhere. Rendering with %v mirrors what the wire encoder
// would serialise.
func leakCheck(t *testing.T, label string, got any, secret string) {
	t.Helper()
	if s := renderAny(got); strings.Contains(s, secret) {
		t.Errorf("%s: secret %q survived scrubbing: %s", label, secret, s)
	}
}

func renderAny(v any) string {
	return strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf("%v", v), "\n", " "))
}

// scrubFields must drop a sensitive KEY at every nesting level, not only
// at the top — a bot's WithFields routinely carries a header dump, a
// nested payload, or a struct with fielded credentials.
func TestScrubFieldsRedactsNestedStructures(t *testing.T) {
	type creds struct {
		Password string
		Note     string
	}
	fields := map[string]any{
		"extra": map[string]any{
			"password":      "hunter2plain",
			"authorization": "Basic Zm9vOmJhcg==",
			"note":          "fine",
		},
		"batch": []any{
			map[string]any{"api_key": "MySecretValue123"},
		},
		"headers": map[string][]string{
			"X-Auth-Token": {"tok-nested-abcdef"},
			"Accept":       {"application/json"},
		},
		"cfg": creds{Password: "structsecret99", Note: "ok"},
	}
	out := scrubFields(fields)
	leakCheck(t, "nested map value under sensitive key", out, "hunter2plain")
	leakCheck(t, "nested slice-of-maps value", out, "MySecretValue123")
	leakCheck(t, "map[string][]string header value", out, "tok-nested-abcdef")
	leakCheck(t, "struct field named Password", out, "structsecret99")
	// The non-sensitive neighbours must SURVIVE — over-redaction destroys
	// the event's value.
	if s := renderAny(out); !strings.Contains(s, "fine") || !strings.Contains(s, "application/json") || !strings.Contains(s, "ok") {
		t.Errorf("over-redaction: benign nested values were destroyed: %s", s)
	}
}

// "author"/"pr_author" must never be eaten by the sensitive-key check:
// bare "auth" as a substring fragment would redact them, and a filter's
// false positive destroys the event's value as surely as a leak.
func TestScrubFieldsKeepsAuthorFields(t *testing.T) {
	out := scrubFields(map[string]any{
		"pr_author": "jo",
		"nested":    map[string]any{"author": "devthejo"},
	})
	if s := renderAny(out); !strings.Contains(s, "jo") || !strings.Contains(s, "devthejo") {
		t.Errorf("author fields were over-redacted: %s", s)
	}
}

// A pathological self-referencing / very deep value must neither recurse
// forever nor leak: past the depth cap everything is flattened through
// Redact.
func TestScrubFieldsBoundsRecursion(t *testing.T) {
	deep := map[string]any{}
	cur := deep
	for i := 0; i < 40; i++ {
		next := map[string]any{}
		cur["d"] = next
		cur = next
	}
	cur["leaf"] = "sk-deepsecret1234567"
	out := scrubFields(map[string]any{"root": deep}) // must return, not hang
	leakCheck(t, "deep leaf provider token", out, "sk-deepsecret1234567")

	loop := map[string]any{}
	loop["self"] = loop
	_ = scrubFields(map[string]any{"root": loop}) // must terminate
}

// scrubEvent must cover the identity/meta surfaces too: ServerName and
// Environment are caller-supplied free-form strings that land on EVERY
// event unfiltered.
func TestScrubEventCoversMetaSurfaces(t *testing.T) {
	ev := &sentry.Event{
		ServerName:  "runner sk-live-abcdef01234567",
		Environment: "env xai-envtok-98765432",
		Release:     "rel ghp_relsecret1234567890",
		Transaction: "txn Bearer abcdefgh12345678",
		Logger:      "logger iap_logtoken12345678",
		Dist:        "dist iwh_disttoken12345678",
		Fingerprint: []string{"fp glpat-fptoken12345678"},
	}
	got := scrubEvent(ev, nil)
	for label, s := range map[string]string{
		"ServerName":  got.ServerName,
		"Environment": got.Environment,
		"Release":     got.Release,
		"Transaction": got.Transaction,
		"Logger":      got.Logger,
		"Dist":        got.Dist,
	} {
		if strings.Contains(s, "secret") || strings.Contains(s, "sk-live") ||
			strings.Contains(s, "xai-envtok") || strings.Contains(s, "Bearer abcdefgh") ||
			strings.Contains(s, "iap_") || strings.Contains(s, "iwh_") {
			t.Errorf("scrubEvent left a credential in %s: %q", label, s)
		}
	}
	leakCheck(t, "Fingerprint", got.Fingerprint, "glpat-fptoken12345678")
	// A normal release string must pass through untouched.
	normal := scrubEvent(&sentry.Event{Release: "iterion@v3.48.3+ed7f6ecad"}, nil)
	if normal.Release != "iterion@v3.48.3+ed7f6ecad" {
		t.Errorf("over-redaction: benign release rewritten to %q", normal.Release)
	}
}

// Redact must catch the industry-common token shapes beyond the provider
// prefixes already covered.
func TestRedactCommonSecretShapes(t *testing.T) {
	cases := map[string]string{
		"slack":     "hook https://hooks.slack.com/services/T0/B0 xoxb-1234567890-abcdefghij",
		"google":    "key AIzaSyA1234567890abcdefghijklmnopqrstuvw",
		"jwt":       "hdr eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"kv-token":  "curl 'https://x.example/cb?token=ThisIsMyGiantSecret123'",
		"kv-apikey": "retrying with api_key=sk_test_notaprefixmatch99",
	}
	for label, in := range cases {
		out := Redact(in)
		if out == in {
			t.Errorf("%s: Redact left the credential untouched: %q", label, in)
		}
	}
	// Anchors must not eat ordinary prose.
	benign := "budget=12 tokens=48657 model=claude-opus-5 https://docs.sentry.io/x"
	if got := Redact(benign); got != benign {
		t.Errorf("over-redaction of benign prose: %q -> %q", benign, got)
	}
}
