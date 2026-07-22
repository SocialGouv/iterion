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
	"github.com/SocialGouv/iterion/pkg/store"
)

// writeThroughRun fakes a copy-based sandbox driver (kubernetes): it
// implements both refresher interfaces and records what was pushed —
// including the stdin payload WriteFileExec streams.
type writeThroughRun struct {
	sandbox.Run
	workspaceFiles map[string][]byte // relPath → value (RefreshWorkspaceFile)
	workspaceErr   error             // when set, RefreshWorkspaceFile fails
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
	if f.workspaceErr != nil {
		return f.workspaceErr
	}
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

	if err := r.writeThroughSandboxGitCredential("run-1", "https://github.com/org/repo.git", "rotated-tok"); err != nil {
		t.Fatalf("write-through: %v", err)
	}

	got, ok := fake.workspaceFiles[".git/"+gitCredentialFile]
	if !ok {
		t.Fatalf("no workspace write-through recorded: %+v", fake.workspaceFiles)
	}
	if want := "https://oauth2:rotated-tok@github.com\n"; string(got) != want {
		t.Errorf("written credential line = %q, want %q", got, want)
	}

	// A transient pod exec failure must SURFACE — the caller keeps the
	// rotation pending and retries next tick instead of recording it done.
	fake.workspaceErr = context.DeadlineExceeded
	if err := r.writeThroughSandboxGitCredential("run-1", "https://github.com/org/repo.git", "t2"); err == nil {
		t.Fatal("expected the refresher failure to propagate")
	}

	// Unknown run / no sandbox: nil no-op, never a panic.
	if err := r.writeThroughSandboxGitCredential("run-absent", "https://github.com/org/repo.git", "t"); err != nil {
		t.Fatalf("absent run must be a nil no-op, got %v", err)
	}

	// A driver without the WorkspaceFileRefresher capability (docker's
	// bind-mounted workspace shares the host inode) is a clean nil no-op.
	r.registerSandboxRun("run-2", fakeSandboxRun{})
	defer r.unregisterSandboxRun("run-2")
	if err := r.writeThroughSandboxGitCredential("run-2", "https://github.com/org/repo.git", "t"); err != nil {
		t.Fatalf("non-refresher driver must be a nil no-op, got %v", err)
	}
}

// TestRefreshGitCredentialsOnce_PartialDeliveryRetries pins the rotation
// bookkeeping: when the host rewrite succeeds but the sandbox
// write-through fails transiently, `last` must NOT advance — otherwise
// the pod keeps the previous token until the NEXT server-side rotation
// (~1h away). The unchanged `last` makes the next tick retry the whole
// rotation; once the write-through succeeds, `last` advances and the
// rotation stops re-applying.
func TestRefreshGitCredentialsOnce_PartialDeliveryRetries(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	mem := secrets.NewMemoryGenericSecretStore()
	id := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(sealer, id, []byte("token-v2"))
	if err != nil {
		t.Fatal(err)
	}
	tctx := store.WithTenant(context.Background(), "team-1")
	if err := mem.Create(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed}); err != nil {
		t.Fatal(err)
	}

	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		Sealer:         sealer,
		GenericSecrets: mem,
	}}
	fake := newWriteThroughRun()
	fake.workspaceErr = context.DeadlineExceeded // pod exec transiently down
	r.registerSandboxRun("run-1", fake)
	defer r.unregisterSandboxRun("run-1")

	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(gitDir, gitCredentialFile)
	const repoURL = "https://github.com/org/repo.git"

	// Tick 1: rotation reaches the host but NOT the pod → last stays "".
	last := ""
	r.refreshGitCredentialsOnce(context.Background(), "team-1", id, "run-1", path, repoURL, &last)
	if last != "" {
		t.Fatalf("last advanced on a partial delivery: %q", last)
	}
	if got, _ := os.ReadFile(path); string(got) != "https://oauth2:token-v2@github.com\n" {
		t.Fatalf("host credential file = %q, want the rotated line", got)
	}

	// Tick 2: pod exec recovered → the SAME rotation is retried and lands
	// everywhere; only now does last advance.
	fake.workspaceErr = nil
	r.refreshGitCredentialsOnce(context.Background(), "team-1", id, "run-1", path, repoURL, &last)
	if last != "token-v2" {
		t.Fatalf("last = %q after full delivery, want token-v2", last)
	}
	if got := string(fake.workspaceFiles[".git/"+gitCredentialFile]); got != "https://oauth2:token-v2@github.com\n" {
		t.Fatalf("pod credential copy = %q, want the rotated line", got)
	}

	// Tick 3: nothing new — no re-push to the pod.
	fake.workspaceFiles = map[string][]byte{}
	r.refreshGitCredentialsOnce(context.Background(), "team-1", id, "run-1", path, repoURL, &last)
	if len(fake.workspaceFiles) != 0 {
		t.Fatalf("unchanged rotation re-pushed to the pod: %+v", fake.workspaceFiles)
	}
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
