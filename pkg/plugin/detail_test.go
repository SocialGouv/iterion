package plugin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetailFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	dir := filepath.Join(home, pluginsSubdir, "full-demo")
	writeFileT(t, filepath.Join(dir, ManifestFile), `
name: full-demo
auto_index: true
contributes:
  rewriters:
    - id: full-demo
      sandbox_mount: /usr/local/bin/full-demo
      locate: { bin: full-demo }
      invoke:
        argv: ["squeeze", "{{command}}"]
        timeout_ms: 250
  mcp_servers:
    - { name: kg, command: full-demo, args: [serve] }
  skills:
    - skills/deep/how-to.md
  commands:
    - commands/ship.md
  agents:
    - agents/reviewer.md
  hooks:
    - hooks/pretooluse.json
  lifecycle:
    index: full-demo index {{workspace}}
    refresh: full-demo refresh
`)
	writeFileT(t, filepath.Join(dir, "hooks", "pretooluse.json"),
		`{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "full-demo hook '{{workspace}}'"}]}]}}`)
	writeFileT(t, filepath.Join(dir, "README.md"), "# full-demo\n")
	writeFileT(t, filepath.Join(dir, "skills", "deep", "how-to.md"), "# how\n")
	writeFileT(t, filepath.Join(dir, "commands", "ship.md"), "# ship\n")
	writeFileT(t, filepath.Join(dir, "agents", "reviewer.md"), "# reviewer\n")

	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d, err := reg.DetailFor("full-demo")
	if err != nil {
		t.Fatalf("DetailFor: %v", err)
	}
	if d.View.Name != "full-demo" || !d.AutoIndex || d.Dir != dir {
		t.Errorf("View/AutoIndex/Dir wrong: %+v", d)
	}
	if d.Readme != "# full-demo\n" {
		t.Errorf("Readme = %q", d.Readme)
	}
	if len(d.Rewriters) != 1 || d.Rewriters[0].ID != "full-demo" ||
		d.Rewriters[0].SandboxMount != "/usr/local/bin/full-demo" ||
		d.Rewriters[0].TimeoutMS != 250 || len(d.Rewriters[0].Argv) != 2 {
		t.Errorf("Rewriters = %+v", d.Rewriters)
	}
	if len(d.MCPServers) != 1 || d.MCPServers[0].Name != "kg" ||
		d.MCPServers[0].Transport != "stdio" || d.MCPServers[0].Command != "full-demo" {
		t.Errorf("MCPServers = %+v", d.MCPServers)
	}
	// Contributed files project as base names.
	if len(d.Skills) != 1 || d.Skills[0] != "how-to.md" {
		t.Errorf("Skills = %v", d.Skills)
	}
	if len(d.Commands) != 1 || d.Commands[0] != "ship.md" {
		t.Errorf("Commands = %v", d.Commands)
	}
	if len(d.Agents) != 1 || d.Agents[0] != "reviewer.md" {
		t.Errorf("Agents = %v", d.Agents)
	}
	// The raw hook shell command is surfaced verbatim under its event.
	if len(d.Hooks) != 1 || d.Hooks[0].Event != "PreToolUse" ||
		len(d.Hooks[0].Commands) != 1 || d.Hooks[0].Commands[0] != "full-demo hook '{{workspace}}'" {
		t.Errorf("Hooks = %+v", d.Hooks)
	}
	if d.Lifecycle == nil || d.Lifecycle.Index != "full-demo index {{workspace}}" || d.Lifecycle.Refresh != "full-demo refresh" {
		t.Errorf("Lifecycle = %+v", d.Lifecycle)
	}

	// JSON projection uses snake_case keys.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"auto_index"`, `"mcp_servers"`, `"sandbox_mount"`, `"timeout_ms"`, `"event"`, `"commands"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON missing key %s: %s", key, raw)
		}
	}
}

func TestDetailForUnknownAndBuiltin(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := reg.DetailFor("no-such-plugin"); err == nil {
		t.Error("unknown plugin accepted")
	}
	// A builtin's detail reads from the embedded FS (rtk ships no README).
	d, err := reg.DetailFor("rtk")
	if err != nil {
		t.Fatalf("DetailFor(rtk): %v", err)
	}
	if !d.View.Builtin || d.Dir != "" || d.Readme != "" {
		t.Errorf("builtin detail wrong: builtin=%v dir=%q readme=%q", d.View.Builtin, d.Dir, d.Readme)
	}
	if len(d.Rewriters) != 1 || d.Rewriters[0].ID != "rtk" {
		t.Errorf("rtk rewriter projection = %+v", d.Rewriters)
	}
}
