package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// writeThroughRun fakes a copy-based sandbox driver (kubernetes): it
// implements both refresher interfaces and records what was pushed —
// including the stdin payload WriteFileExec streams.
type writeThroughRun struct {
	sandbox.Run
	workspaceFiles map[string][]byte // relPath → value (RefreshWorkspaceFile)
	secretFiles    map[string][]byte // name → value (RefreshSecretFile)
	execStdin      [][]byte          // payloads streamed to Exec (WriteFileExec)
	execPaths      []string          // $1 of each Exec invocation
}

func newWriteThroughRun() *writeThroughRun {
	return &writeThroughRun{
		workspaceFiles: map[string][]byte{},
		secretFiles:    map[string][]byte{},
	}
}

func (f *writeThroughRun) Driver() string { return "fake-k8s" }

func (f *writeThroughRun) RefreshWorkspaceFile(_ context.Context, relPath string, value []byte) error {
	f.workspaceFiles[relPath] = append([]byte(nil), value...)
	return nil
}

func (f *writeThroughRun) RefreshSecretFile(_ context.Context, name string, value []byte) error {
	f.secretFiles[name] = append([]byte(nil), value...)
	return nil
}

func (f *writeThroughRun) Exec(_ context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if opts.Stdin != nil {
		b, _ := io.ReadAll(opts.Stdin)
		f.execStdin = append(f.execStdin, b)
	}
	if len(cmd) > 0 {
		f.execPaths = append(f.execPaths, cmd[len(cmd)-1])
	}
	return sandbox.ExecResult{}, nil
}

// TestWriteThroughSandboxGitCredential proves a rotated forge token is
// pushed into the sandbox workspace's credential-store copy — the file
// the k8s pod's git actually reads (the host clone rewrite alone never
// reaches the tar-copied workspace).
func TestWriteThroughSandboxGitCredential(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelError, os.Stderr)}}
	fake := newWriteThroughRun()
	r.registerSandboxRun("run-1", fake)
	defer r.unregisterSandboxRun("run-1")

	r.writeThroughSandboxGitCredential("run-1", "https://github.com/org/repo.git", "rotated-tok")

	got, ok := fake.workspaceFiles[".git/"+gitCredentialFile]
	if !ok {
		t.Fatalf("no workspace write-through recorded: %+v", fake.workspaceFiles)
	}
	if want := "https://oauth2:rotated-tok@github.com\n"; string(got) != want {
		t.Errorf("written credential line = %q, want %q", got, want)
	}

	// Unknown run / no sandbox: silent no-op, never a panic.
	r.writeThroughSandboxGitCredential("run-absent", "https://github.com/org/repo.git", "t")

	// A driver without the WorkspaceFileRefresher capability (docker's
	// bind-mounted workspace shares the host inode) is a clean no-op.
	r.registerSandboxRun("run-2", fakeSandboxRun{})
	defer r.unregisterSandboxRun("run-2")
	r.writeThroughSandboxGitCredential("run-2", "https://github.com/org/repo.git", "t")
}

// TestPropagateForfaitToSandbox proves a refreshed forfait credentials
// file reaches BOTH in-sandbox locations: the ADR-070 secret mount
// (RefreshSecretFile) and the writable seeded CLAUDE_CONFIG_DIR copy
// (streamed over stdin, never argv).
func TestPropagateForfaitToSandbox(t *testing.T) {
	var logBuf bytes.Buffer
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelInfo, &logBuf)}}
	fake := newWriteThroughRun()
	r.registerSandboxRun("run-1", fake)
	defer r.unregisterSandboxRun("run-1")

	path := filepath.Join(t.TempDir(), ".credentials.json")
	payload := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-FRESH"}}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	r.propagateForfaitToSandbox("run-1", path)

	if got := string(fake.secretFiles[secrets.ClaudeCodeOAuthSecretName]); got != payload {
		t.Errorf("secret-mount refresh = %q, want the refreshed payload", got)
	}
	if len(fake.execStdin) != 1 || string(fake.execStdin[0]) != payload {
		t.Fatalf("config-copy write-through stdin = %v, want exactly one payload", fake.execStdin)
	}
	if len(fake.execPaths) != 1 || fake.execPaths[0] != secrets.ClaudeCodeSandboxCredentialsPath {
		t.Errorf("config-copy target = %v, want %q", fake.execPaths, secrets.ClaudeCodeSandboxCredentialsPath)
	}
	if strings.Contains(logBuf.String(), "sk-ant-oat-FRESH") {
		t.Error("token bytes leaked into the log")
	}

	// No registered sandbox / non-refresher driver: silent no-ops.
	r.propagateForfaitToSandbox("run-absent", path)
	r.registerSandboxRun("run-2", fakeSandboxRun{})
	defer r.unregisterSandboxRun("run-2")
	r.propagateForfaitToSandbox("run-2", path)
}

func TestRenderGitCredentialLine(t *testing.T) {
	line, err := renderGitCredentialLine("https://gitlab.example.com/g/p.git", "tok")
	if err != nil || line != "https://oauth2:tok@gitlab.example.com\n" {
		t.Fatalf("line = %q, err = %v", line, err)
	}
	if _, err := renderGitCredentialLine("not a url", "tok"); err == nil {
		t.Error("expected an error for a hostless repo URL")
	}
}
