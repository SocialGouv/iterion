package cli

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// isolatedSecretStore points the local store and its master key at a
// throwaway directory, so a test never reads the operator's own
// ~/.iterion/secrets.json nor their OS keychain.
func isolatedSecretStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ITERION_HOME", dir)
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	return dir
}

// setSecret runs the CLI set path with the value handed over in an env
// var (never argv, never stdin).
func setSecret(t *testing.T, name, kind, value string) error {
	t.Helper()
	t.Setenv("ITER_TEST_SECRET_VALUE", value)
	p := &Printer{W: &bytes.Buffer{}, Format: OutputHuman}
	return RunSecretSet(p, SecretOptions{Name: name, Kind: kind, FromEnv: "ITER_TEST_SECRET_VALUE"})
}

// storedSecret reports whether the local store holds name.
func storedSecret(t *testing.T, name string) bool {
	t.Helper()
	st, err := LocalSecretStores("")
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	_, ok := st.Global().GetByName(name)
	return ok
}

// The API paths refuse credential material whose shape cannot
// authenticate; `iterion secret set` is the same door one over. A
// transcript pasted under a credential name must be refused at
// ingestion, with the wording the API gives — not sealed and discovered
// as a provider 401 at the first run.
func TestRunSecretSet_RefusesATranscriptPaste(t *testing.T) {
	isolatedSecretStore(t)

	value := "\x1b[32mWelcome to Claude Code\x1b[0m\nsk-ant-oat01-secret"
	err := setSecret(t, "ANTHROPIC_AUTH_TOKEN", "", value)
	if err == nil {
		t.Fatal("a terminal transcript was accepted as a token")
	}
	// The reason is the API's, verbatim — one rule, two doors.
	want := secrets.ValidateTokenShape("ANTHROPIC_AUTH_TOKEN", value)
	if want == nil {
		t.Fatal("fixture no longer trips the shape gate")
	}
	if !strings.Contains(err.Error(), want.(*secrets.ShapeError).Reason) {
		t.Fatalf("error = %q, want the API's reason %q", err, want.(*secrets.ShapeError).Reason)
	}
	// The remedy has to be reachable from the message alone.
	if !strings.Contains(err.Error(), "--kind raw") {
		t.Fatalf("error = %q, want it to name the explicit opt-out", err)
	}
	if storedSecret(t, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatal("a refused value was stored")
	}
}

// The gate must not turn the generic store into a token-only store: it
// legitimately holds PEM keys and JSON documents, and `--kind raw` is
// the explicit opt-out for anything else.
func TestRunSecretSet_AcceptsTheShapesTheStoreIsFor(t *testing.T) {
	isolatedSecretStore(t)

	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nZmFrZS1rZXktbWF0ZXJpYWw=\n-----END OPENSSH PRIVATE KEY-----"
	cases := []struct {
		name  string
		kind  string
		value string
	}{
		{"BEARER_TOKEN", "", "sk-test-0123456789abcdef"},
		{"DEPLOY_KEY", "", pem},
		{"SERVICE_ACCOUNT", "", `{"type":"service_account","project_id":"p"}`},
		{"PASSPHRASE", "raw", "correct horse battery staple"},
		{"EXPLICIT_PEM", "pem", pem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := setSecret(t, tc.name, tc.kind, tc.value); err != nil {
				t.Fatalf("set %s: %v", tc.name, err)
			}
			if !storedSecret(t, tc.name) {
				t.Fatalf("%s was accepted but not stored", tc.name)
			}
		})
	}
}

// An explicit --kind overrides the inference: `--kind token` is how an
// operator says "this one really is a bare token" about a value the
// inference would have waved through as a JSON document.
func TestRunSecretSet_ExplicitKindOverridesTheInference(t *testing.T) {
	isolatedSecretStore(t)

	// A pasted credentials.json — which the inference reads as json and
	// accepts — is refused once the operator says it is a token.
	pastedFile := "{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"sk-ant-oat01-x\"\n  }\n}"
	if err := setSecret(t, "STILL_JSON", "", pastedFile); err != nil {
		t.Fatalf("guard: the inference must accept the file as json: %v", err)
	}
	if err := setSecret(t, "NOT_A_TOKEN", "token", pastedFile); err == nil {
		t.Fatal("--kind token accepted a multi-line JSON document")
	}
	if err := setSecret(t, "TRUNCATED_JSON", "json", `{"tokens":{"acce`); err == nil {
		t.Fatal("--kind json accepted a truncated paste")
	}
	if err := setSecret(t, "NOT_A_PEM", "pem", "sk-test-0123456789abcdef"); err == nil {
		t.Fatal("--kind pem accepted a bare token")
	}
	for _, n := range []string{"NOT_A_TOKEN", "TRUNCATED_JSON", "NOT_A_PEM"} {
		if storedSecret(t, n) {
			t.Fatalf("%s was refused but stored", n)
		}
	}
}

// An unknown --kind is a typo, not a silent pass-through to no checking.
func TestRunSecretSet_RefusesAnUnknownKind(t *testing.T) {
	isolatedSecretStore(t)

	err := setSecret(t, "SOME_SECRET", "tokne", "sk-test-0123456789abcdef")
	if err == nil {
		t.Fatal("an unknown --kind was accepted — the gate silently did nothing")
	}
	if !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "raw") {
		t.Fatalf("error = %q, want it to list the accepted kinds", err)
	}
	if storedSecret(t, "SOME_SECRET") {
		t.Fatal("a secret with an unknown kind was stored")
	}
}
