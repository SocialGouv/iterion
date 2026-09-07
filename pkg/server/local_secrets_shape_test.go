package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The studio's Secrets view is the SECOND door into the local generic store,
// and it was the ungated one: `iterion secret set` refuses a value that could
// not possibly authenticate (a terminal transcript pasted as a token, a
// truncated JSON credential document), while the same paste through the studio
// landed on disk and surfaced hours later as a provider 401 mid-run.
func TestLocalSecretsRefuseAValueThatCannotAuthenticate(t *testing.T) {
	e := newLocalSecretsE2E(t)

	cases := []struct {
		name string
		body string
		want string // a fragment the refusal must carry
	}{
		{
			"a pasted transcript is not a bearer token",
			`{"name":"forge_token","secret":"ghp_abc123\nghp_second_line"}`,
			"newline",
		},
		{
			"a truncated JSON credential document",
			`{"name":"dependabot_tokens","kind":"json","secret":"{\"my-org\": \"github_pat_x"}`,
			"JSON document",
		},
		{
			"an empty JSON credential document carries nothing",
			`{"name":"dependabot_tokens","kind":"json","secret":"{}"}`,
			"empty JSON document",
		},
		{
			"a truncated PEM block",
			`{"name":"app_key","kind":"pem","secret":"-----BEGIN RSA PRIVATE KEY-----\nMIIE"}`,
			"PEM block",
		},
		{
			"an unknown kind is an error, never a silent pass-through",
			`{"name":"whatever","kind":"nonsense","secret":"abc"}`,
			"unknown secret kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.call(t, http.MethodPost, "/api/local/secrets", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body = %s", code, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body = %s, want it to name %q", body, tc.want)
			}
			if strings.Contains(string(body), "ghp_abc123") || strings.Contains(string(body), "github_pat_x") {
				t.Error("the refusal must never echo the value")
			}
		})
	}
}

// `raw` is the escape hatch the gate needs to be usable: a passphrase, a
// connection string, a blob is none of the three checked shapes, and without
// an opt-out gating the studio door would make them unstorable.
func TestLocalSecretsRawKindStoresAnythingAndTheGateNamesIt(t *testing.T) {
	e := newLocalSecretsE2E(t)

	code, body := e.call(t, http.MethodPost, "/api/local/secrets",
		`{"name":"db_password","kind":"raw","secret":"correct horse battery staple"}`)
	if code != http.StatusOK {
		t.Fatalf("raw must store unchecked: code = %d body = %s", code, body)
	}

	// And the refusal on the default path points at it, so the remedy travels
	// with the message.
	code, body = e.call(t, http.MethodPost, "/api/local/secrets",
		`{"name":"db_password2","secret":"correct horse battery staple"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body = %s", code, body)
	}
	if !strings.Contains(string(body), "raw") {
		t.Errorf("body = %s, want the raw opt-out named", body)
	}
}

// The gate is on the update door too: a rotation is a fresh paste.
func TestLocalSecretsUpdateAppliesTheShapeGate(t *testing.T) {
	e := newLocalSecretsE2E(t)

	code, body := e.call(t, http.MethodPost, "/api/local/secrets", `{"name":"forge_token","secret":"ghp_valid_token"}`)
	if code != http.StatusOK {
		t.Fatalf("seed: code = %d body = %s", code, body)
	}
	var created localSecretView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	code, body = e.call(t, http.MethodPatch, "/api/local/secrets/"+created.ID,
		`{"secret":"ghp_rotated\ntrailing"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("rotation must be gated: code = %d body = %s", code, body)
	}
	if !strings.Contains(string(body), "newline") {
		t.Errorf("body = %s, want the shape refusal", body)
	}

	// A rename with no new value is not a paste and must still work.
	code, body = e.call(t, http.MethodPatch, "/api/local/secrets/"+created.ID, `{"name":"forge_token_renamed"}`)
	if code != http.StatusOK {
		t.Fatalf("rename without a value: code = %d body = %s", code, body)
	}
}
