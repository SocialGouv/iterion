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

// TestUltra11yMCPPinMatchesEngineDefault: mcp_server args are literals —
// {{vars.engine_version}} is not expanded. A --var override cannot retarget
// the MCP server, so the default pin and the args list must stay in lockstep.
func TestUltra11yMCPPinMatchesEngineDefault(t *testing.T) {
	data, err := os.ReadFile("ultra11y/main.bot")
	if err != nil {
		t.Fatalf("read ultra11y/main.bot: %v", err)
	}
	src := string(data)
	def := ""
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "engine_version:") && strings.Contains(line, "=") {
			// engine_version: string = "2.32.0"
			i := strings.Index(line, `"`)
			j := strings.LastIndex(line, `"`)
			if i >= 0 && j > i {
				def = line[i+1 : j]
			}
		}
	}
	if def == "" {
		t.Fatal("ultra11y/main.bot: could not read engine_version default")
	}
	want := `args: ["-y", "ultra11y@` + def + `", "mcp"]`
	if !strings.Contains(src, want) {
		t.Fatalf("ultra11y/main.bot: mcp_server args must pin ultra11y@%s (literal, no var expansion); missing %q", def, want)
	}
}
