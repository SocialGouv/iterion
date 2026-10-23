package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// copySandboxRun stands for the copy-based transport (the k8s path): the child
// reads its OWN copy of the workspace, so a `sh` script the engine runs inside
// it acts on that copy and not on the host tree.
//
// Here the copy is a real directory and the script really runs, which is what
// makes the difference between the two transports observable at all. Every
// fixture covering child resources runs with `sandbox: none`, so nothing in
// the suite exercised this path — and a divergence nothing exercises is one
// nobody measures.
type copySandboxRun struct{ root string }

func (copySandboxRun) Driver() string { return "copy-test" }

func (copySandboxRun) Command(ctx context.Context, cmd []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, cmd[0], cmd[1:]...)
}

func (r copySandboxRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := r.Command(ctx, cmd, opts)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	err := c.Run()
	res := sandbox.ExecResult{Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return res, err
}

func (copySandboxRun) Cleanup(context.Context) error { return nil }

// The refresher is what makes the engine take the copy path at all
// (sharedSandboxIsCopyBased is a type assertion on it).
func (r copySandboxRun) RefreshWorkspaceFile(_ context.Context, rel string, value []byte) error {
	dst := filepath.Join(r.root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, value, 0o600)
}

func writeClaudeFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheCopyAChildReadsMatchesTheHostAfterAdoption.
//
// adoptSharedSandbox runs TWO steps: clearBorrowedSandboxResources wipes the
// four borrowed entries in the copy, and writeThroughMirroredSkills then walks
// the host's whole .claude/{skills,commands,agents} plus settings.json and
// pushes every file back. Judging either half alone says nothing about what
// the child reads.
//
// This drives the pair, which is the only thing that answers the question the
// two transports are compared on: does the child see the workspace's own
// resources?
func TestTheCopyAChildReadsMatchesTheHostAfterAdoption(t *testing.T) {
	host, copyRoot := t.TempDir(), t.TempDir()
	workspace := []string{
		".claude/settings.json",
		".claude/commands/review.md",
		".claude/agents/scout.md",
		".claude/skills/hand-written/SKILL.md",
	}
	for _, rel := range workspace {
		writeClaudeFile(t, host, rel, "workspace")
		writeClaudeFile(t, copyRoot, rel, "workspace")
	}
	// A parent file the host side already hid (hideParentSkillCollisions runs
	// before adoption): present in the copy, absent from the host.
	writeClaudeFile(t, copyRoot, ".claude/skills/from-parent/SKILL.md", "parent")

	run := copySandboxRun{root: copyRoot}
	e := &Engine{
		workDir:       host,
		sharedSandbox: &SharedSandbox{Run: run, WorkspaceFolder: copyRoot},
		resourceScope: &runResourceScope{path: host, borrowed: true, owned: map[string]string{}},
	}
	if err := e.clearBorrowedSandboxResources(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	n, werr := writeThroughMirroredSkills(context.Background(), host, run, nil)
	if werr != nil {
		t.Fatalf("write-through: %v", werr)
	}
	if n != len(workspace) {
		t.Fatalf("wrote %d files back, want %d", n, len(workspace))
	}

	for _, rel := range workspace {
		body, err := os.ReadFile(filepath.Join(copyRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("the child's copy lost %s, which the workspace owns: %v", rel, err)
			continue
		}
		if string(body) != "workspace" {
			t.Errorf("%s = %q, want the workspace's bytes", rel, body)
		}
	}
	// And a parent file the host no longer has does not survive in the copy:
	// the child would otherwise read a resource its parent contributed.
	if _, err := os.Stat(filepath.Join(copyRoot, ".claude", "skills", "from-parent", "SKILL.md")); err == nil {
		t.Error("the child's copy kept a parent file the host had already hidden")
	}
}

// A subbot child holding a sealed gateway credential cannot borrow the
// parent's sandbox: the parent's proxy pins the PARENT's gateway host, and
// the child's key would egress past a pin set that never named it (an open
// parent: no proxy at all). The child resolves a sandbox of its own, whose
// proxy pins its own credential.
func TestShouldAdoptSharedSandbox_AChildWithASealedPairResolvesItsOwn(t *testing.T) {
	e := &Engine{workflow: &ir.Workflow{}}
	e.sharedSandbox = &SharedSandbox{Run: copySandboxRun{root: t.TempDir()}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "sk-child"},
		APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://child-gw.example"},
	})
	var events []map[string]any
	adopt, err := e.shouldAdoptSharedSandbox(ctx, func(t store.EventType, data map[string]any) error {
		events = append(events, data)
		return nil
	})
	// Not an error: the child FALLS THROUGH to a sandbox of its own, whose
	// proxy pins its own credential — a refusal here would kill the run
	// instead. The sandbox_shared event names why the parent was passed.
	if err != nil {
		t.Fatalf("the credentialed child's non-adoption killed the run: %v", err)
	}
	if adopt {
		t.Fatal("a credentialed child adopted the parent's sandbox — its key egresses past a pin set that never named it")
	}
	if len(events) == 0 {
		t.Fatal("no sandbox_shared event names why the parent was passed")
	}
}

// The alias refusal judges the CANONICAL host — the dial rewrites casings
// and a root dot away before its own local-destination guard sees it, so a
// hand-built credential naming the alias in any spelling must be refused at
// the read (rva r3 R1: the raw comparison let two spellings tunnel to the
// host's loopback, the pin never consulted).
func TestShouldAdoptSharedSandbox_ANDResolve_AliasHostRefusedInAnySpelling(t *testing.T) {
	for _, host := range []string{"Host.Docker.Internal", "host.docker.internal."} {
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "sk-run"},
			APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://" + host + ":8443/v1"},
		})
		creds, ok := secrets.CredentialsFromContext(ctx)
		if !ok {
			t.Fatal("scenario broken: no creds")
		}
		_, baseURL, err := creds.GatewayCredentialForRoute(secrets.ProviderOpenAICompatible)
		if err != nil || baseURL == "" {
			t.Fatal("scenario broken: no pair")
		}
		// The read hands the guard what URL.Host preserved — the case and
		// the root dot INCLUDED; the guard does its own canonicalising.
		runGatewayHost := (compatgw.Config{BaseURL: baseURL}).Host()
		if err := refuseRunGatewayAlias(runGatewayHost); err == nil {
			t.Errorf("the alias host %q was not refused — the spelling escapes the guard", runGatewayHost)
		}
	}
}
