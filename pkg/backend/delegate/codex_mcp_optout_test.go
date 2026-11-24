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
// servers its CODEX_HOME's mcp.json lists, and records, for that listing and
// for every other spawn (the SDK's app-server), the CODEX_HOME and working
// directory it ran with, before failing the app-server.
const codexStandInCLI = `#!/bin/sh
case "$1" in
--version) echo "codex-cli 0.156.1"; exit 0 ;;
mcp) printf 'mcp home=%s pwd=%s\n' "$CODEX_HOME" "$(pwd -P)" >> "$CODEX_ARGV_LOG"; cat "$CODEX_HOME/mcp.json"; exit 0 ;;
esac
printf 'run home=%s pwd=%s %s\n' "$CODEX_HOME" "$(pwd -P)" "$*" >> "$CODEX_ARGV_LOG"
exit 3
`

// TestCodexRefusesAnOptedOutNodeItsConfigurationGivesServers runs the real
// codex backend against a stand-in CLI. A node that opted out of the ambient
// MCP set, on a codex whose own configuration lists an enabled server, is
// refused typed before codex runs — codex cannot be told to leave that
// server off (TestCodexAppServerRefusesAStringForABoolean). With none
// enabled, or for a node that did not opt out, codex runs.
//
// The listing is asked of the codex the node runs: under the spawn's own
// CODEX_HOME — the default ambient policy gives the spawn an overlay home,
// not the process's — and in the node's working directory, where the
// app-server runs.
//
// Mutation that reddens it: drop the MCPInheritDisabled block of
// CodexBackend.Execute, or the refusal on a non-empty list; hand
// codexConfiguredMCPServers no environment, or drop its cmd.Dir.
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
			home := filepath.Join(dir, "codex-home")
			mustWriteFile(t, filepath.Join(home, "mcp.json"), tc.list)
			// Instruction files in the home: the default ambient policy then
			// serves the spawn an overlay home without them.
			mustWriteFile(t, filepath.Join(home, "AGENTS.md"), "operator instructions\n")
			t.Setenv("CODEX_HOME", home)
			t.Setenv("HOME", t.TempDir())
			work, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			b := &CodexBackend{Command: cli, Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
			_, err = b.Execute(context.Background(), Task{NodeID: "converse_agent", WorkDir: work, UserPrompt: "x", AmbientContext: ambient.Workspace, MCPInheritDisabled: tc.optOut})
			raw, _ := os.ReadFile(logPath)
			var listed, ran []string
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				kind, rest, _ := strings.Cut(line, " ")
				where := strings.Join(strings.Fields(rest)[:2], " ")
				switch kind {
				case "mcp":
					listed = append(listed, where)
				case "run":
					ran = append(ran, where)
				}
			}
			var refusal *ErrCapabilityUnsupported
			if refused := errors.As(err, &refusal); refused != tc.refused {
				t.Fatalf("refused = %v (err %v), want %v", refused, err, tc.refused)
			}
			if tc.refused && (len(ran) != 0 || !strings.Contains(err.Error(), "probe, projprobe")) {
				t.Errorf("a refused node still ran codex (%q) or the refusal names no server: %v", ran, err)
			}
			if !tc.refused && len(ran) == 0 {
				t.Errorf("codex never ran: %v\n%s", err, raw)
			}
			if asked := len(listed) > 0; asked != tc.optOut {
				t.Fatalf("codex was asked for its MCP servers: %v, want %v (%q)", asked, tc.optOut, listed)
			}
			for _, l := range listed {
				if !strings.HasSuffix(l, "pwd="+work) {
					t.Errorf("the listing ran in %q, not the node's working directory %s", l, work)
				}
				if strings.HasPrefix(l, "home="+home+" ") {
					t.Errorf("the listing read the process's CODEX_HOME (%s), not the spawn's: %q", home, l)
				}
				for _, r := range ran {
					if r != l {
						t.Errorf("the listing ran as %q, the app-server as %q: the listing does not read the codex the node runs", l, r)
					}
				}
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
