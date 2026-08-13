package bots

import (
	"os"
	"strings"
	"testing"
)

// TestUltra11yAuditArgsIsStringArray guards the empty-scope façade a live
// run of Ally produced: prepare used to emit audit_args as a space-joined
// string (`studio/src/** --graph`), the tool-node interpolator single-quotes
// a string as ONE argv, and the engine then treated `--graph` as part of the
// glob — 0 files, 100% conformance, which is exactly what a clean repo
// looks like. string[] expands to one quoted token per element.
func TestUltra11yAuditArgsIsStringArray(t *testing.T) {
	data, err := os.ReadFile("ultra11y/main.bot")
	if err != nil {
		t.Fatalf("read ultra11y/main.bot: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "audit_args:     string[]") && !strings.Contains(src, "audit_args:    string[]") {
		t.Fatal("ultra11y/main.bot: audit_args must be string[] on prepare_output / audit_input — a joined string is interpolated as one shell token and the engine reports 0 files")
	}
	if strings.Contains(src, `'audit_args': ' '.join(args)`) || strings.Contains(src, `"audit_args": " ".join(args)`) {
		t.Fatal("ultra11y/main.bot: prepare must emit audit_args as a list, not a space-joined string")
	}
}
