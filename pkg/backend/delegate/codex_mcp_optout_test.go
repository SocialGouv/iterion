package delegate

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// codexStandInCLI answers the version probe and `mcp list --json` with the
// servers $CODEX_MCP_LIST names, and records every other spawn — the SDK's
// app-server — before failing it.
const codexStandInCLI = `#!/bin/sh
case "$1" in
--version) echo "codex-cli 0.156.1"; exit 0 ;;
mcp) printf 'mcp %s\n' "$*" >> "$CODEX_ARGV_LOG"; printf '%s\n' "$CODEX_MCP_LIST"; exit 0 ;;
esac
printf 'run %s\n' "$*" >> "$CODEX_ARGV_LOG"
exit 3
`

// TestCodexRefusesAnOptedOutNodeItsConfigurationGivesServers runs the real
// codex backend against a stand-in CLI. A node that opted out of the ambient
// MCP set, on a codex whose own configuration lists an enabled server, is
// refused typed before codex runs — codex cannot be told to leave that
// server off (TestCodexAppServerRefusesAStringForABoolean). With none
// enabled, or for a node that did not opt out, codex runs.
//
// Mutation that reddens it: drop the MCPInheritDisabled block of
// CodexBackend.Execute, or the refusal on a non-empty list.
func TestCodexRefusesAnOptedOutNodeItsConfigurationGivesServers(t *testing.T) {
	enabled := `[{"name":"probe","enabled":true},{"name":"projprobe","enabled":true}]`
	for _, tc := range []struct {
		name    string
		optOut  bool
		list    string
		refused bool
	}{
		{"opted out, servers configured", true, enabled, true},
		{"opted out, every server disabled", true, `[{"name":"probe","enabled":false}]`, false},
		{"control: inherits", false, enabled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cli := filepath.Join(dir, "codex")
			if err := os.WriteFile(cli, []byte(codexStandInCLI), 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "argv.log")
			t.Setenv("CODEX_ARGV_LOG", logPath)
			t.Setenv("CODEX_MCP_LIST", tc.list)
			t.Setenv("CODEX_HOME", filepath.Join(dir, "codex-home"))
			t.Setenv("HOME", t.TempDir())
			b := &CodexBackend{Command: cli, Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
			_, err := b.Execute(context.Background(), Task{NodeID: "converse_agent", WorkDir: t.TempDir(), UserPrompt: "x", AmbientContext: ambient.All, MCPInheritDisabled: tc.optOut})
			raw, _ := os.ReadFile(logPath)
			ran := strings.Contains(string(raw), "run ")
			var refusal *ErrCapabilityUnsupported
			if refused := errors.As(err, &refusal); refused != tc.refused {
				t.Fatalf("refused = %v (err %v), want %v", refused, err, tc.refused)
			}
			if tc.refused && (ran || !strings.Contains(err.Error(), "probe, projprobe")) {
				t.Errorf("a refused node still ran codex (%v) or the refusal names no server: %v\n%s", ran, err, raw)
			}
			if !tc.refused && !ran {
				t.Errorf("codex never ran: %v\n%s", err, raw)
			}
		})
	}
}

// TestCodexAppServerRefusesAStringForABoolean pins the measurement the
// refusal stands on, with the REAL codex CLI (skipped without one): its
// configuration's servers are listed from the node's working directory — the
// operator's CODEX_HOME server and a trusted project's — and the pinned SDK's
// only override channel, a string-valued map, cannot disable one: the
// app-server refuses `"false"` for `mcp_servers.<name>.enabled`.
func TestCodexAppServerRefusesAStringForABoolean(t *testing.T) {
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "codex-home")
	repo := filepath.Join(tmp, "repo")
	mustWriteFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(repo, ".codex", "config.toml"), "[mcp_servers.projprobe]\ncommand = \"/bin/true\"\n")
	mustWriteFile(t, filepath.Join(home, "config.toml"), "[mcp_servers.probe]\ncommand = \"/bin/true\"\n\n[projects.\""+repo+"\"]\ntrust_level = \"trusted\"\n")
	names, err := codexConfiguredMCPServers(context.Background(), cli, map[string]string{"CODEX_HOME": home}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"probe", "projprobe"}) {
		t.Fatalf("listed %v, want [probe projprobe]", names)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var queryErr error
	// A system prompt, as Execute always sets one: the SDK then runs the
	// app-server, the transport every codex node of the engine takes.
	for _, qerr := range codexsdk.Query(ctx, codexsdk.Text("x"),
		codexsdk.WithSystemPrompt(codexOutputDisciplinePreamble),
		codexsdk.WithCliPath(cli), codexsdk.WithCwd(repo),
		codexsdk.WithEnv(map[string]string{"CODEX_HOME": home, "OPENAI_API_KEY": "sk-invalid"}),
		codexsdk.WithConfig(map[string]string{"mcp_servers.probe.enabled": "false"})) {
		if qerr != nil {
			queryErr = qerr
			break
		}
	}
	if queryErr == nil || !strings.Contains(queryErr.Error(), "expected a boolean") {
		t.Fatalf("the app-server took a string for a boolean (err %v): the refusal's premise no longer holds — wire the opt-out instead", queryErr)
	}
}
