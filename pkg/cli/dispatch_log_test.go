package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
)

// The dispatcher daemon ships its logs, so its default format is JSON —
// the same contract as `iterion server` and `iterion runner`, and the
// reason a log shipper can read a dispatcher pod with no extra config.
func TestDispatchLogConfig_DefaultsToJSON(t *testing.T) {
	t.Setenv("ITERION_LOG_FORMAT", "")
	t.Setenv("ITERION_LOG_LEVEL", "")

	cfg, err := dispatchLogConfig()
	if err != nil {
		t.Fatalf("dispatchLogConfig: %v", err)
	}
	if cfg.Format != iterconfig.LogFormatJSON {
		t.Fatalf("format = %q, want json", cfg.Format)
	}

	var buf bytes.Buffer
	cfg.NewLogger(&buf).WithField("issue_id", "abc").Info("dispatcher: claimed")

	var rec struct {
		TS     string         `json:"ts"`
		Level  string         `json:"level"`
		Msg    string         `json:"msg"`
		Fields map[string]any `json:"fields"`
	}
	line := strings.TrimSpace(buf.String())
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("dispatcher line is not JSON (%v): %q", err, line)
	}
	if rec.TS == "" || rec.Level != "info" || rec.Msg != "dispatcher: claimed" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Fields["issue_id"] != "abc" {
		t.Fatalf("fields = %+v", rec.Fields)
	}
}

// The default is a default, never a cage.
func TestDispatchLogConfig_EnvOverrides(t *testing.T) {
	t.Setenv("ITERION_LOG_FORMAT", "human")
	t.Setenv("ITERION_LOG_LEVEL", "debug")

	cfg, err := dispatchLogConfig()
	if err != nil {
		t.Fatalf("dispatchLogConfig: %v", err)
	}
	if cfg.Format != iterconfig.LogFormatHuman {
		t.Fatalf("format = %q, want human", cfg.Format)
	}

	var buf bytes.Buffer
	logger := cfg.NewLogger(&buf)
	logger.Debug("visible at debug")
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("human format emitted JSON: %q", out)
	}
	if !strings.Contains(out, "visible at debug") {
		t.Fatalf("ITERION_LOG_LEVEL=debug did not take effect: %q", out)
	}
}

func TestDispatchLogConfig_InvalidFormatIsAnError(t *testing.T) {
	t.Setenv("ITERION_LOG_FORMAT", "yaml")
	if _, err := dispatchLogConfig(); err == nil {
		t.Fatal("an invalid ITERION_LOG_FORMAT must be reported, not silently ignored")
	}
}
