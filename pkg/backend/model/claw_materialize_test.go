package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
)

// claw materialises a secret into the tool input's DECODED strings, like
// claude_code: swapped into the JSON text, a quote broke the call, a literal
// backslash-n became a newline (the wrong credential), and a value ending a
// string then writing `,"command":…` replaced the command itself.
func TestClawMaterializesASecretByteForByte(t *testing.T) {
	const call = `{"command":"curl -sf -H 'Authorization: Bearer __ITERION_SECRET_API_TOKEN__' https://api.example.test/v1/me","timeout_ms":12345678901234567890}`
	for _, secret := range []string{`abc"def`, `pa\ns`, `x","command":"id; echo INJECTED #`, "<tag>&amp;"} {
		g := secretguard.New([]secretguard.Secret{{Name: "API_TOKEN", Value: secret}}, secretguard.DefaultConfig())
		var raw json.RawMessage
		gt := &GenerationTool{Name: "Bash", Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			raw = append(json.RawMessage(nil), input...)
			return "ok", nil
		}}
		if _, err := runToolExecution(context.Background(), gt, toolUseBlock{ID: "tu1", Name: "Bash", PartialJSON: call}, g.Materialize, nil, nil); err != nil {
			t.Fatalf("secret %q: %v", secret, err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("secret %q: the tool got invalid JSON %s: %v", secret, raw, err)
		}
		want := "curl -sf -H 'Authorization: Bearer " + secret + "' https://api.example.test/v1/me"
		if args["command"] != want || len(args) != 2 {
			t.Fatalf("secret %q: the tool got %#v", secret, args)
		}
		if !strings.Contains(string(raw), "12345678901234567890") {
			t.Fatalf("secret %q: a number lost its digits: %s", secret, raw)
		}
	}
}
