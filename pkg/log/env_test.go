package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewFromEnv_DefaultsToHuman(t *testing.T) {
	t.Setenv(EnvFormat, "")
	t.Setenv(EnvLevel, "")

	var buf bytes.Buffer
	NewFromEnv(&buf).Info("hello")
	out := strings.TrimSpace(buf.String())
	if strings.HasPrefix(out, "{") {
		t.Fatalf("default format is not human: %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("message lost: %q", out)
	}
}

func TestNewFromEnv_HonoursFormatAndLevel(t *testing.T) {
	t.Setenv(EnvFormat, "JSON")
	t.Setenv(EnvLevel, "debug")

	var buf bytes.Buffer
	NewFromEnv(&buf).Debug("shipped")
	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "{") || !strings.Contains(out, `"level":"debug"`) {
		t.Fatalf("ITERION_LOG_FORMAT/_LEVEL not honoured: %q", out)
	}
}

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Format
	}{
		{"json", FormatJSON},
		{" JSON ", FormatJSON},
		{"human", FormatHuman},
		{"", FormatHuman},
		{"yaml", FormatHuman},
	} {
		if got := ParseFormat(tc.in); got != tc.want {
			t.Errorf("ParseFormat(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
