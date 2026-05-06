package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
)

func TestRedactJSONTextField_DropsText(t *testing.T) {
	in := []byte(`{"text":"alice@example.com","mode":"redact","min_score":0.7}`)
	out := redactJSONTextField(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, out)
	}
	if got := m["text"].(string); got != privacy.EventTextMarker {
		t.Fatalf("text not redacted: %q", got)
	}
	if got := m["mode"].(string); got != "redact" {
		t.Fatalf("mode altered: %q", got)
	}
	if got := m["min_score"].(float64); got != 0.7 {
		t.Fatalf("min_score altered: %v", got)
	}
	if strings.Contains(string(out), "alice@example.com") {
		t.Fatalf("output still contains raw PII: %s", out)
	}
}

func TestRedactJSONTextField_NoText(t *testing.T) {
	in := []byte(`{"mode":"detect"}`)
	out := redactJSONTextField(in)
	if string(out) != string(in) {
		t.Fatalf("payload should be unchanged when no text field: got %s", out)
	}
}

func TestRedactJSONTextField_InvalidJSON(t *testing.T) {
	in := []byte(`{not json}`)
	out := redactJSONTextField(in)
	if string(out) != string(in) {
		t.Fatalf("invalid JSON should be returned untouched")
	}
}

func TestRedactJSONTextField_PreservesSiblings(t *testing.T) {
	in := []byte(`{"text":"Hello alice@example.com","substituted":["PII_aaaaaaaa"],"missing":[]}`)
	out := redactJSONTextField(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if got := m["text"].(string); got != privacy.EventTextMarker {
		t.Fatalf("text not redacted: %q", got)
	}
	subs, ok := m["substituted"].([]any)
	if !ok || len(subs) != 1 || subs[0].(string) != "PII_aaaaaaaa" {
		t.Fatalf("substituted should be preserved: %+v", subs)
	}
	if strings.Contains(string(out), "alice@example.com") {
		t.Fatalf("output still contains raw PII: %s", out)
	}
}
