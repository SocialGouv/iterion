package secrets

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The ASCII-only gate let every non-ASCII white-space and format rune
// through, and an NBSP-glued token was sealed through the chokepoint
// (#627 round 1). Each rune below must be refused, naming itself and its
// position, and the error must be the typed ShapeError the handlers map
// to 400 — never the value.
func TestValidateTokenShape_RefusesUnicodeWhitespaceAndInvisibles(t *testing.T) {
	runes := []struct {
		name string
		r    rune
	}{
		{"NO-BREAK SPACE", 0x00A0},
		{"NEXT LINE", 0x0085},
		{"ZERO WIDTH SPACE", 0x200B},
		{"LINE SEPARATOR", 0x2028},
		{"PARAGRAPH SEPARATOR", 0x2029},
		{"IDEOGRAPHIC SPACE", 0x3000},
		{"ZERO WIDTH NO-BREAK SPACE (BOM)", 0xFEFF},
	}
	for _, tc := range runes {
		t.Run(tc.name, func(t *testing.T) {
			value := "sk-ant-oat01-good" + string(tc.r) + "tail"
			err := ValidateTokenShape("accessToken", value)
			if err == nil {
				t.Fatalf("U+%04X was accepted", tc.r)
			}
			var se *ShapeError
			if !errors.As(err, &se) {
				t.Fatalf("got %T, want *ShapeError so the handler maps it to 400", err)
			}
			want := fmt.Sprintf("U+%04X", tc.r)
			if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "position 17") {
				t.Fatalf("error %q must name %s and position 17", err.Error(), want)
			}
			if strings.Contains(err.Error(), "sk-ant-oat01-good") {
				t.Fatalf("error leaks the value: %q", err.Error())
			}
		})
	}
	if err := ValidateTokenShape("accessToken", "sk-ant-\xff-bad"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 must be refused as such, got %v", err)
	}
	if err := ValidateTokenShape("accessToken", "sk-ant-oat01-fine-héllo"); err != nil {
		t.Fatalf("a printable non-ASCII letter is a legal token character: %v", err)
	}
}

// Bedrock and Vertex hold a JSON credential document, not a bearer token
// (documented on ApiKey, offered by the studio picker, listed in
// docs/byok.md). Refusing them as "a terminal transcript" for containing
// newlines was round 1's H3; the shape rule is per provider.
func TestValidateAPIKeyShape_PerProvider(t *testing.T) {
	awsBlob := "{\n  \"aws_access_key_id\": \"AKIA...\",\n  \"aws_secret_access_key\": \"x\"\n}"
	cases := []struct {
		name     string
		provider Provider
		value    string
		wantErr  string
	}{
		{"anthropic bearer ok", ProviderAnthropic, "sk-ant-api03-realkey", ""},
		{"anthropic with a space refused", ProviderAnthropic, "sk-ant good", "space"},
		{"anthropic with NBSP refused", ProviderAnthropic, "sk-ant\u00a0good", "U+00A0"},
		{"bedrock JSON object ok", ProviderBedrock, awsBlob, ""},
		{"vertex JSON object ok", ProviderVertex, "{\"type\":\"service_account\",\"project_id\":\"p\"}", ""},
		{"bedrock bearer-looking refused", ProviderBedrock, "AKIAIOSFODNN7EXAMPLE", "JSON credential object"},
		{"bedrock JSON array refused", ProviderBedrock, "[1,2]", "JSON credential object"},
		{"bedrock truncated JSON refused", ProviderBedrock, "{\"aws_access_key_id\":", "JSON credential object"},
		{"vertex empty refused", ProviderVertex, "   ", "empty"},
		{"bedrock empty JSON object refused", ProviderBedrock, "{}", "empty JSON object"},
		{"vertex empty JSON object refused", ProviderVertex, "{ }", "empty JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAPIKeyShape(tc.provider, tc.value)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			var se *ShapeError
			if !errors.As(err, &se) {
				t.Fatalf("got %T, want *ShapeError", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q, want it to mention %q", err.Error(), tc.wantErr)
			}
		})
	}
	if ProviderAnthropic.CredentialIsJSON() || ProviderOpenAI.CredentialIsJSON() || ProviderZAI.CredentialIsJSON() {
		t.Fatal("bearer providers must not be JSON-credential providers")
	}
}

const testPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nZmFrZS1rZXktbWF0ZXJpYWw=\n-----END OPENSSH PRIVATE KEY-----\n"

// The generic secret store holds PEM keys and JSON documents as well as
// bearer tokens, so the shape read off a value decides which gate runs.
func TestInferSecretShapeKind(t *testing.T) {
	cases := []struct {
		value string
		want  SecretShapeKind
	}{
		{"sk-ant-0123456789", SecretShapeToken},
		{testPEM, SecretShapePEM},
		{"  " + testPEM, SecretShapePEM},
		{`{"type":"service_account"}`, SecretShapeJSON},
		{"\n[\"a\",\"b\"]", SecretShapeJSON},
		// A transcript is not a document, and must fall to the token rule
		// that refuses it — never to raw.
		{"Welcome to Claude Code\nsk-ant-0123", SecretShapeToken},
	}
	for _, tc := range cases {
		if got := InferSecretShapeKind(tc.value); got != tc.want {
			t.Errorf("InferSecretShapeKind(%.20q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestParseSecretShapeKind(t *testing.T) {
	for _, want := range SecretShapeKinds {
		got, err := ParseSecretShapeKind(strings.ToUpper(string(want)))
		if err != nil || got != want {
			t.Errorf("ParseSecretShapeKind(%q) = (%q, %v), want %q", want, got, err, want)
		}
	}
	// An unknown kind is a typo, never a silent pass-through to no check.
	_, err := ParseSecretShapeKind("tokne")
	if err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	for _, k := range SecretShapeKinds {
		if !strings.Contains(err.Error(), string(k)) {
			t.Fatalf("error %q must list %q so the typo is fixable from the message", err, k)
		}
	}
	// The empty string is "infer" — the caller's business, not an error
	// this function invents a default for.
	if _, err := ParseSecretShapeKind(""); err == nil {
		t.Fatal("the empty kind must be rejected here; inference is the caller's decision")
	}
}

func TestValidateSecretShape(t *testing.T) {
	cases := []struct {
		name    string
		kind    SecretShapeKind
		value   string
		wantErr string
	}{
		{"token: clean", SecretShapeToken, "sk-ant-0123456789", ""},
		{"token: transcript", SecretShapeToken, "Welcome\nsk-ant-0123", "newline"},
		{"json: object", SecretShapeJSON, `{"type":"service_account"}`, ""},
		{"json: array", SecretShapeJSON, `["a"]`, ""},
		{"json: truncated", SecretShapeJSON, `{"tokens":{"acce`, "not a JSON document"},
		{"json: empty object", SecretShapeJSON, `{}`, "empty JSON document"},
		{"json: empty array", SecretShapeJSON, `[]`, "empty JSON document"},
		{"json: a bare token", SecretShapeJSON, "sk-ant-0123", "not a JSON document"},
		{"pem: block", SecretShapePEM, testPEM, ""},
		{"pem: truncated", SecretShapePEM, "-----BEGIN OPENSSH PRIVATE KEY-----\nZmFrZQ==", "not a PEM block"},
		// raw is the explicit opt-out: it must accept what every other
		// gate refuses, or it is not an escape hatch.
		{"raw: a whole transcript", SecretShapeRaw, "\x1b[32mWelcome\x1b[0m\nanything at all", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSecretShape(tc.kind, "MY_SECRET", tc.value)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var se *ShapeError
			if !errors.As(err, &se) {
				t.Fatalf("got %v (%T), want a *ShapeError", err, err)
			}
			if se.Field != "MY_SECRET" {
				t.Errorf("field = %q, want the secret's name so the operator knows which one", se.Field)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q, want it to mention %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Fatalf("the refusal echoed the value: %q", err)
			}
		})
	}
}
