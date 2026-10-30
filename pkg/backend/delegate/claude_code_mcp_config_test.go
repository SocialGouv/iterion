package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The per-node host-MCP token must not ride a spawn's argv.
//
// `--mcp-config` carries the whole resolved MCP catalog inline, each http
// server's `headers` included — and `X-Iterion-Run` is in there. Since #2758
// that token spends the tenant's connector credentials through the facade, so
// what sat in /proc/<pid>/cmdline (mode 444, readable by every user of the
// host) for the life of the node was a credential. The sandboxed spawn is the
// one that matters most: that is the only shape the connector facade is wired
// for, and its inline document rides the host's `docker exec` argv.
//
// Mutation that reddens these (producer): drop the `rest, err =
// f.pinMCPConfig(rest)` arm from pinHost, or the `pinSandboxMCPConfig` call
// from claudeSandboxSpawnArgv.

const mcpProbeToken = "run-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func mcpProbeDoc(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		connectorMCPServerName: map[string]any{
			"type":    "http",
			"url":     "http://127.0.0.1:1/api/v1/mcp/connectors",
			"headers": map[string]string{"X-Iterion-Run": mcpProbeToken},
		},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestAHostSpawnReadsItsMCPCatalogFromAPrivateFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	settings := []byte(`{"env":{"A":"1"}}`)
	doc := mcpProbeDoc(t)

	var files claudeSettingsFiles
	cmd := exec.Command("true", "--print", "--settings", string(settings), "--mcp-config", doc, "prompt")
	files.pinHost(cmd, settings)
	if cmd.Err != nil {
		t.Fatalf("pinHost: %v", cmd.Err)
	}

	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, mcpProbeToken) {
		t.Errorf("the run token is still on argv: %q", joined)
	}
	i := slices.Index(cmd.Args, "--mcp-config")
	if i < 0 || i+1 >= len(cmd.Args) {
		t.Fatalf("argv lost its --mcp-config: %q", cmd.Args)
	}
	path := cmd.Args[i+1]
	if !strings.HasPrefix(path, tmp) {
		t.Fatalf("--mcp-config = %q, want a file under %s", path, tmp)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the catalog back: %v", err)
	}
	if string(got) != doc {
		t.Errorf("the file holds %q, want the document the SDK built", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — the file replaces an argv every user could read", info.Mode().Perm())
	}
	// The prompt and the rest of the argv survive the move.
	if !slices.Contains(cmd.Args, "prompt") || !slices.Contains(cmd.Args, "--print") {
		t.Errorf("argv lost arguments in the move: %q", cmd.Args)
	}

	// The pass that owns the spawn takes BOTH files with it.
	files.remove(nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the catalog file outlived the pass: %v", err)
	}
}

// writerSandboxRun is a sandbox double that RUNS the in-sandbox writer on the
// host, so the file sandbox.WriteFileExec produces is the real one — content
// over stdin, 0600, under the umask the writer sets.
type writerSandboxRun struct {
	sandbox.Run
	// root stands in for the container's /tmp: the writer's absolute
	// destination is rebased under it, so the real writer script runs and the
	// file it produces stays inside the test's own temp dir.
	root  string
	argvs [][]string
}

func (r *writerSandboxRun) hostPath(absPath string) string {
	return filepath.Join(r.root, filepath.Base(absPath))
}

func (r *writerSandboxRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	r.argvs = append(r.argvs, slices.Clone(argv))
	argv = slices.Clone(argv)
	if r.root != "" && len(argv) >= 6 && argv[0] == "sh" && argv[3] == "sh" {
		argv[4] = r.hostPath(argv[4])
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := sandbox.ExecResult{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func TestASandboxedSpawnReadsItsMCPCatalogFromAnInContainerFile(t *testing.T) {
	tmp := t.TempDir()
	settings := []byte(`{"env":{"A":"1"}}`)
	doc := mcpProbeDoc(t)
	run := &writerSandboxRun{root: tmp}

	// The mark decides the in-container path; the double rebases it under the
	// test's temp dir so the real writer script runs and nothing is written
	// outside what this test owns.
	mark := "probe"
	args := []string{"--print", "--settings", string(settings), "--mcp-config", doc, "prompt"}
	// Through the WIRING both spawn paths of this backend call, not through
	// the helper alone: a test of the helper is green while the spawn still
	// carries the document.
	rest, err := claudeSandboxSpawnArgv(context.Background(), run, mark, "claude", args, settings)
	if err != nil {
		t.Fatalf("claudeSandboxSpawnArgv: %v", err)
	}

	joined := strings.Join(rest, " ")
	if strings.Contains(joined, mcpProbeToken) {
		t.Errorf("the run token is still on the argv handed to `docker exec`: %q", joined)
	}
	if i := slices.Index(rest, "--mcp-config"); i < 0 || rest[i+1] != sandboxMCPConfigFile(mark) {
		t.Fatalf("argv = %q, want --mcp-config %s", rest, sandboxMCPConfigFile(mark))
	}
	// The wrapper still carries the CLI and the prompt: the move must not eat
	// the spawn.
	if !slices.Contains(rest, "claude") || !slices.Contains(rest, "prompt") {
		t.Errorf("argv lost the spawn in the move: %q", rest)
	}
	// The DOCUMENT must have travelled over stdin, never on the writer's own
	// argv: that argv is the `docker exec` client's, on the host, which is
	// the surface this whole move is about.
	if len(run.argvs) != 1 {
		t.Fatalf("the writer ran %d times, want once", len(run.argvs))
	}
	if w := strings.Join(run.argvs[0], " "); strings.Contains(w, mcpProbeToken) {
		t.Errorf("the writer carried the token on ITS argv: %q", w)
	}
	// And the file it wrote holds the document, owner-only.
	written, err := os.ReadFile(run.hostPath(sandboxMCPConfigFile(mark)))
	if err != nil {
		t.Fatalf("read the in-sandbox catalog: %v", err)
	}
	if string(written) != doc {
		t.Errorf("the file holds %q, want the document the SDK built", written)
	}
	info, err := os.Stat(run.hostPath(sandboxMCPConfigFile(mark)))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	// The spawn's cleanup list names it, or the file outlives the node.
	if !slices.Contains(sandboxSpawnFiles(mark), sandboxMCPConfigFile(mark)) {
		t.Error("sandboxSpawnFiles omits the catalog, so the exec that ends the spawn leaves it in the container")
	}
}

// Both argv spellings, and the shapes that must NOT be rewritten: a value
// that is already a path is the caller's own file, and an argv with no
// catalog at all is a node with no MCP server.
func TestTakeInlineMCPConfigReadsBothSpellingsAndLeavesAPathAlone(t *testing.T) {
	doc := `{"mcpServers":{}}`
	for _, tc := range []struct {
		name  string
		args  []string
		found bool
		rest  []string
	}{
		{"separate", []string{"--print", "--mcp-config", doc, "p"}, true, []string{"--print", "p"}},
		{"equals", []string{"--print", "--mcp-config=" + doc, "p"}, true, []string{"--print", "p"}},
		{"already a path", []string{"--print", "--mcp-config", "/tmp/x.json", "p"}, false, []string{"--print", "--mcp-config", "/tmp/x.json", "p"}},
		{"absent", []string{"--print", "p"}, false, []string{"--print", "p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, found := takeInlineMCPConfig(tc.args)
			if found != tc.found {
				t.Fatalf("found = %v, want %v", found, tc.found)
			}
			if found && got != doc {
				t.Errorf("doc = %q, want %q", got, doc)
			}
			if !slices.Equal(rest, tc.rest) {
				t.Errorf("rest = %q, want %q", rest, tc.rest)
			}
		})
	}
}

// A document the CLI would reject, and a write that fails, both REFUSE the
// spawn: falling back to the inline argv would put the token back exactly
// when something is already wrong.
func TestASandboxedSpawnRefusesRatherThanFallBackToArgv(t *testing.T) {
	if _, err := pinSandboxMCPConfig(context.Background(), &writerSandboxRun{}, "probe",
		[]string{"--mcp-config", "{not json"}); err == nil {
		t.Error("a malformed catalog was accepted")
	}
	// A nil run is what WriteFileExec refuses; the refusal must reach the
	// caller rather than leaving the document on argv.
	rest, err := pinSandboxMCPConfig(context.Background(), nil, "probe", []string{"--mcp-config", `{"mcpServers":{}}`})
	if err == nil {
		t.Errorf("a sandbox that cannot be written to was accepted: %q", rest)
	}
	if rest != nil {
		t.Errorf("rest = %q, want nil on refusal", rest)
	}
}
