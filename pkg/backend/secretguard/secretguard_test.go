package secretguard

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

const (
	// A realistic-length fake secret (40 chars) so encodings are long
	// and unique enough to taint safely.
	fakeKey = "sk-ant-FAKE-abcDEF0123456789ghiJKLmnoPQRස" // includes a non-ASCII rune on purpose
	awsKey  = "AKIAIOSFODNN7EXAMPLE"                      // canonical AWS example access key
)

func newTestGuard(t *testing.T, secrets ...Secret) *Guard {
	t.Helper()
	return New(secrets, DefaultConfig())
}

func TestEncodingsOf_CoversFormats(t *testing.T) {
	v := "MyS3cretValue-0123456789"
	encs := encodingsOf(v)
	want := map[string]string{
		"raw":        v,
		"base64 std": base64.StdEncoding.EncodeToString([]byte(v)),
		"base64 url": base64.URLEncoding.EncodeToString([]byte(v)),
		"hex lower":  hex.EncodeToString([]byte(v)),
		"hex upper":  strings.ToUpper(hex.EncodeToString([]byte(v))),
		"url query":  url.QueryEscape(v),
	}
	set := make(map[string]struct{}, len(encs))
	for _, e := range encs {
		set[e] = struct{}{}
	}
	for label, form := range want {
		if _, ok := set[form]; !ok {
			t.Errorf("encodingsOf missing %s form %q", label, form)
		}
	}
}

func TestRedact_KnownValueAllEncodings(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "api_key", Value: fakeKey})
	ph := defaultPlaceholder("api_key")

	cases := map[string]string{
		"raw":        fakeKey,
		"base64 std": base64.StdEncoding.EncodeToString([]byte(fakeKey)),
		"base64 raw": base64.RawStdEncoding.EncodeToString([]byte(fakeKey)),
		"base64 url": base64.URLEncoding.EncodeToString([]byte(fakeKey)),
		"hex":        hex.EncodeToString([]byte(fakeKey)),
		"hex upper":  strings.ToUpper(hex.EncodeToString([]byte(fakeKey))),
		"url query":  url.QueryEscape(fakeKey),
	}
	for label, encoded := range cases {
		in := "prefix " + encoded + " suffix"
		got := g.Redact(in)
		if strings.Contains(got, encoded) && encoded != ph {
			t.Errorf("%s: secret still present after Redact: %q", label, got)
		}
		if !strings.Contains(got, ph) {
			t.Errorf("%s: expected placeholder %q in %q", label, ph, got)
		}
	}
}

func TestRedact_JSONEscapedValue(t *testing.T) {
	// A value with characters that JSON escapes, embedded inside a JSON
	// document the way it would appear in events.jsonl.
	v := `line1
"quoted"\back`
	g := newTestGuard(t, Secret{Name: "tok", Value: v})
	doc := `{"field":"` + jsonEscape(v) + `"}`
	got := g.Redact(doc)
	if strings.Contains(got, jsonEscape(v)) {
		t.Errorf("json-escaped secret survived: %q", got)
	}
	if !strings.Contains(got, defaultPlaceholder("tok")) {
		t.Errorf("expected placeholder in %q", got)
	}
}

func jsonEscape(v string) string {
	// mirror encodingsOf's json form
	for _, e := range encodingsOf(v) {
		if e != v && strings.Contains(e, `\`) {
			return e
		}
	}
	return v
}

func TestMaterialize_RoundTrip(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "deploy_key", Value: fakeKey})
	ph := defaultPlaceholder("deploy_key")
	cmd := `curl -H "Authorization: Bearer ` + ph + `" https://api.example.com`
	got := g.Materialize(cmd)
	if !strings.Contains(got, fakeKey) {
		t.Errorf("Materialize did not substitute real value: %q", got)
	}
	if strings.Contains(got, ph) {
		t.Errorf("placeholder survived Materialize: %q", got)
	}
	// Redact is the inverse on the materialised text.
	if back := g.Redact(got); !strings.Contains(back, ph) || strings.Contains(back, fakeKey) {
		t.Errorf("Redact did not invert Materialize: %q", back)
	}
}

// TestMaterializeShell_SingleQuoteInjection guards the fix for the deepsec
// finding (run 019f02f4): the tool-node template wraps a secret placeholder in
// single quotes, and plain Materialize substituted the RAW value, so a secret
// value containing a single quote broke out of the quoting -> shell injection.
// MaterializeShell escapes the value for inside-single-quote use so the
// surrounding quotes stay balanced and the value is inert shell text.
func TestMaterializeShell_SingleQuoteInjection(t *testing.T) {
	// A hostile secret value that, raw, would close the quote and run `id`.
	const evil = `x'; id #`
	g := newTestGuard(t, Secret{Name: "tok", Value: evil})
	ph := defaultPlaceholder("tok")
	// The template layer emits the placeholder inside single quotes.
	cmd := `deploy --token '` + ph + `'`

	raw := g.Materialize(cmd)
	if !strings.Contains(raw, `'; id #`) {
		t.Fatalf("precondition: raw Materialize should embed the unescaped value: %q", raw)
	}

	got := g.MaterializeShell(cmd)
	// Every single quote in the VALUE must be the escaped form ('\''), so no
	// bare `'` from the value can terminate the surrounding quote. `sh -c got`
	// then passes the literal value `x'; id #` as one arg — `id` never runs.
	want := `deploy --token 'x'\''; id #'`
	if got != want {
		t.Errorf("MaterializeShell = %q; want %q", got, want)
	}
	if strings.Contains(got, ph) {
		t.Errorf("placeholder survived MaterializeShell: %q", got)
	}
}

func TestContainsSecret_DeterministicGate(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "k", Value: fakeKey})
	if !g.ContainsSecret("payload=" + fakeKey) {
		t.Error("ContainsSecret should match raw value")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(fakeKey))
	if !g.ContainsSecret("blob:" + b64) {
		t.Error("ContainsSecret should match base64 value")
	}
	if g.ContainsSecret("nothing sensitive here, just words and 12345") {
		t.Error("ContainsSecret false positive on benign text")
	}
	// The gate must NOT fire on a heuristic-only token (unknown AWS key).
	if g.ContainsSecret("env AWS_KEY=" + awsKey) {
		t.Error("ContainsSecret must not fire on heuristic-only (unregistered) tokens")
	}
}

func TestFileSecretReferenceRendersPathAndRegistersValue(t *testing.T) {
	g := newTestGuard(t, Secret{
		Name:     "kubeconfig",
		Value:    fakeKey,
		FilePath: "/run/iterion/secrets/kubeconfig",
		Env:      "KUBECONFIG",
	})
	if got := g.ResolveSecretRef("kubeconfig"); got != "/run/iterion/secrets/kubeconfig" {
		t.Fatalf("ResolveSecretRef(file) = %q", got)
	}
	if !g.ContainsSecret("payload=" + fakeKey) {
		t.Fatal("file secret value should remain registered for DLP/redaction")
	}
	hints := g.SecretFileHints()
	if len(hints) != 1 || hints[0].Path != "/run/iterion/secrets/kubeconfig" || hints[0].Env != "KUBECONFIG" {
		t.Fatalf("file hints not preserved: %+v", hints)
	}
}

func TestRedact_HeuristicUnknownToken(t *testing.T) {
	g := newTestGuard(t) // no known secrets
	in := "leaked: " + awsKey + " end"
	got := g.Redact(in)
	if strings.Contains(got, awsKey) {
		t.Errorf("unknown AWS key not redacted heuristically: %q", got)
	}
	if !strings.Contains(got, DefaultConfig().Marker) {
		t.Errorf("expected marker in %q", got)
	}
}

func TestRedact_RecursiveBase64Decode(t *testing.T) {
	g := newTestGuard(t) // no known secrets; relies on recursive decode
	wrapped := base64.StdEncoding.EncodeToString([]byte(awsKey))
	in := "data " + wrapped + " more"
	got := g.Redact(in)
	if strings.Contains(got, wrapped) {
		t.Errorf("base64-wrapped AWS key not caught by recursive decode: %q", got)
	}
	if !strings.Contains(got, DefaultConfig().Marker) {
		t.Errorf("expected marker after recursive decode: %q", got)
	}
}

func TestRedact_DoesNotOverRedactBenign(t *testing.T) {
	g := newTestGuard(t)
	// A 40-char hex commit hash and a base64 of plain English — neither
	// is a token shape; the generic 0.6 rule is below the 0.7 MinScore,
	// so both must survive.
	commit := "9f1c2b3d4e5f60718293a4b5c6d7e8f901234567"
	benignB64 := base64.StdEncoding.EncodeToString([]byte("the quick brown fox jumps over a dog"))
	in := "commit " + commit + " note " + benignB64
	got := g.Redact(in)
	if !strings.Contains(got, commit) {
		t.Errorf("benign commit hash was over-redacted: %q", got)
	}
	if !strings.Contains(got, benignB64) {
		t.Errorf("benign base64 text was over-redacted: %q", got)
	}
}

func TestNilGuard_NoOp(t *testing.T) {
	var g *Guard
	if got := g.Redact("hello " + awsKey); got != "hello "+awsKey {
		t.Errorf("nil Redact mutated input: %q", got)
	}
	if got := g.Materialize("x"); got != "x" {
		t.Errorf("nil Materialize mutated input: %q", got)
	}
	if g.ContainsSecret("x") {
		t.Error("nil ContainsSecret should be false")
	}
	if g.HasKnownSecrets() {
		t.Error("nil HasKnownSecrets should be false")
	}
}

func TestNew_SkipsShortValues(t *testing.T) {
	g := New([]Secret{{Name: "tiny", Value: "ab"}}, DefaultConfig())
	if g.HasKnownSecrets() {
		t.Error("values shorter than MinLen must not be registered")
	}
}

func TestRedact_PreservesExistingPlaceholder(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "k", Value: fakeKey})
	ph := defaultPlaceholder("k")
	in := "already redacted: " + ph + " ok"
	if got := g.Redact(in); !strings.Contains(got, ph) {
		t.Errorf("existing placeholder was clobbered: %q", got)
	}
}

func TestRedact_HeuristicDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Heuristic = false
	g := New(nil, cfg)
	in := "leaked: " + awsKey
	if got := g.Redact(in); !strings.Contains(got, awsKey) {
		t.Errorf("heuristic disabled should leave unknown tokens: %q", got)
	}
}
