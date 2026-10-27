package ir

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCompileUsesExplicitTargetCapabilities(t *testing.T) {
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ITERION_PI_AGENT_DIR", t.TempDir())
	t.Setenv("ITERION_PI_BIN", "")
	t.Setenv("ITERION_OPENCODE_BIN", "")
	available, absent := true, false
	for _, kind := range []string{"agent", "judge", "router"} {
		for _, explicit := range []string{"", "node", "model", "workflow", "workflow-auto"} {
			mode, nodeField, workflowField := "", "", ""
			edges := "  a -> done\n"
			if kind == "router" {
				mode = "  mode: llm\n"
				edges += "  a -> fail\n"
			} else {
				mode = "  output: s\n"
			}
			switch explicit {
			case "node":
				nodeField = "  backend: auto\n"
			case "model":
				nodeField = "  model: \"test/model\"\n"
			case "workflow":
				workflowField = "  default_backend: codex\n"
			case "workflow-auto":
				workflowField = "  default_backend: auto\n"
			}
			file := parseFile(t, fmt.Sprintf("schema s:\n  ok: bool\n\n%s a:\n%s%s\nworkflow w:\n%s  entry: a\n%s", kind, mode, nodeField, workflowField, edges))
			for _, capability := range []struct {
				name  string
				value *bool
			}{{"unknown", nil}, {"available", &available}, {"absent", &absent}} {
				t.Run(kind+"/"+explicit+"/"+capability.name, func(t *testing.T) {
					var first *CompileResult
					for _, credential := range []string{"", "synthetic-test-key"} {
						t.Setenv("ANTHROPIC_API_KEY", credential)
						r := CompileWithCapabilities(file, CompileCapabilities{AutoBackendAvailable: capability.value})
						if first != nil && !reflect.DeepEqual(first, r) {
							t.Fatal("compilation changed with the compiling host's credentials")
						}
						first = r
						fallsThrough := explicit == "" || explicit == "node" || explicit == "workflow-auto"
						if r.HasErrors() != (fallsThrough && capability.name == "absent" && kind != "router") {
							t.Fatalf("unexpected compile verdict: %v", r.Diagnostics)
						}
						var found []Diagnostic
						for _, d := range r.Diagnostics {
							if d.Code == DiagMissingModelOrBackend {
								found = append(found, d)
							}
						}
						// `backend: auto` and `default_backend: auto` fall
						// through the documented chain like no field at all,
						// so they reach the capability branch with the empty
						// explicit value.
						wantCount := 0
						wantSeverity := SeverityWarning
						if fallsThrough && capability.name != "available" {
							wantCount = 1
							if capability.name == "absent" && kind != "router" {
								wantSeverity = SeverityError
							}
						}
						if len(found) != wantCount || (wantCount == 1 && found[0].Severity != wantSeverity) {
							t.Fatalf("C018 = %v, want count %d severity %v", found, wantCount, wantSeverity)
						}
						if wantCount == 1 && (found[0].NodeID != "a" || found[0].File != "test.bot" || found[0].Line != 4) {
							t.Fatalf("C018 must locate the declaration at test.bot:4: %+v", found[0])
						}
						if capability.value == nil && !reflect.DeepEqual(Compile(file), r) {
							t.Fatal("ordinary Compile must use unknown target capabilities")
						}
					}
				})
			}
		}
	}
}
