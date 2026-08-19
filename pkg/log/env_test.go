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

func TestNewFallback_KeepsCallerLevelUntilEnvOverrides(t *testing.T) {
	t.Setenv(EnvFormat, "")
	t.Setenv(EnvLevel, "")

	var buf bytes.Buffer
	l := NewFallback(LevelWarn, &buf)
	l.Info("chatter")
	l.Warn("kept")
	out := buf.String()
	if strings.Contains(out, "chatter") {
		t.Fatalf("caller's warn default not kept: %q", out)
	}
	if !strings.Contains(out, "kept") {
		t.Fatalf("warn line lost: %q", out)
	}

	t.Setenv(EnvLevel, "debug")
	buf.Reset()
	NewFallback(LevelWarn, &buf).Debug("opened up")
	if !strings.Contains(buf.String(), "opened up") {
		t.Fatalf("ITERION_LOG_LEVEL did not override the default: %q", buf.String())
	}
}

func TestNewFallback_HonoursFormat(t *testing.T) {
	t.Setenv(EnvFormat, "json")
	t.Setenv(EnvLevel, "")

	var buf bytes.Buffer
	NewFallback(LevelWarn, &buf).Warn("shipped")
	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "{") || !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("a fallback logger broke the JSON stream: %q", out)
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
