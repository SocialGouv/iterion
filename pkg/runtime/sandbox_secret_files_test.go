package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	secretspkg "github.com/SocialGouv/iterion/pkg/secrets"
)

func TestAddSecretFileMounts_GenericSecret(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {
			As:        "file",
			MountPath: "/run/iterion/secrets/kubeconfig",
			Env:       "KUBECONFIG",
		},
	}}
	ctx := secretspkg.WithCredentials(context.Background(), secretspkg.Credentials{
		Generic: map[string]string{"kubeconfig": "apiVersion: v1"},
	})
	var spec sandbox.Spec
	if err := addSecretFileMounts(ctx, &spec, wf, nil); err != nil {
		t.Fatalf("addSecretFileMounts: %v", err)
	}
	if len(spec.SecretFiles) != 1 {
		t.Fatalf("SecretFiles = %+v", spec.SecretFiles)
	}
	sf := spec.SecretFiles[0]
	if sf.Name != "kubeconfig" || sf.MountPath != "/run/iterion/secrets/kubeconfig" || string(sf.Value) != "apiVersion: v1" {
		t.Fatalf("secret file mount not populated: %+v", sf)
	}
	if spec.Env["KUBECONFIG"] != "/run/iterion/secrets/kubeconfig" {
		t.Fatalf("KUBECONFIG env not injected: %+v", spec.Env)
	}
}

func TestAddSecretFileMounts_ValueExpressionAndDefaultPath(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"deploy/key": {
			As:    "file",
			Value: "{{vars.secret_payload}}",
		},
	}}
	var spec sandbox.Spec
	err := addSecretFileMounts(context.Background(), &spec, wf, map[string]any{"secret_payload": "payload"})
	if err != nil {
		t.Fatalf("addSecretFileMounts: %v", err)
	}
	sf := spec.SecretFiles[0]
	if sf.MountPath != "/run/iterion/secrets/deploy_key" || string(sf.Value) != "payload" {
		t.Fatalf("default path/value mismatch: %+v", sf)
	}
}

func TestAddSecretFileMounts_MissingValueFails(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {As: "file"},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err == nil {
		t.Fatal("expected missing file secret value to fail")
	}
}

func TestAddSecretFileMounts_OptionalUnresolvedSkips(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"gitlab_token": {As: "file", Optional: true},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err != nil {
		t.Fatalf("optional unresolved file secret should be skipped, not error: %v", err)
	}
	if len(spec.SecretFiles) != 0 {
		t.Fatalf("no mount expected, got %d", len(spec.SecretFiles))
	}
}

func TestAddSecretFileMounts_DuplicatePathFails(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"a": {As: "file", Value: "one", MountPath: "/run/iterion/secrets/shared"},
		"b": {As: "file", Value: "two", MountPath: "/run/iterion/secrets/shared"},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err == nil {
		t.Fatal("expected duplicate mount_path to fail")
	}
}

func TestAddSecretFileMounts_DirtyPathFails(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {As: "file", Value: "payload", MountPath: "/run/iterion/secrets/../kubeconfig"},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err == nil {
		t.Fatal("expected dirty mount_path to fail")
	}
}

// fakeSeedRun captures the exec invocation seedClaudeConfigDir issues.
type fakeSeedRun struct {
	sandbox.Run
	gotCmd   []string
	exitCode int
}

func (f *fakeSeedRun) Driver() string { return "fake" }
func (f *fakeSeedRun) Exec(_ context.Context, cmd []string, _ sandbox.ExecOpts) (sandbox.ExecResult, error) {
	f.gotCmd = cmd
	return sandbox.ExecResult{ExitCode: f.exitCode, Stderr: []byte("boom")}, nil
}

func TestAddClaudeOAuthSecretFile(t *testing.T) {
	t.Run("no credentials in ctx is a silent no-op", func(t *testing.T) {
		var spec sandbox.Spec
		added, err := addClaudeOAuthSecretFile(context.Background(), &spec)
		if err != nil || added || len(spec.SecretFiles) != 0 {
			t.Fatalf("added=%v err=%v spec=%+v — want a silent no-op", added, err, spec.SecretFiles)
		}
	})

	t.Run("materialised forfait is mounted on the ADR-070 channel", func(t *testing.T) {
		dir := t.TempDir()
		payload := `{"claudeAiOauth":{"accessToken":"tok"}}`
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := secretspkg.WithCredentials(context.Background(), secretspkg.Credentials{
			OAuthCredentialFiles: map[string]string{string(secretspkg.OAuthKindClaudeCode): dir},
		})
		var spec sandbox.Spec
		added, err := addClaudeOAuthSecretFile(ctx, &spec)
		if err != nil || !added {
			t.Fatalf("added=%v err=%v", added, err)
		}
		if len(spec.SecretFiles) != 1 {
			t.Fatalf("SecretFiles = %+v", spec.SecretFiles)
		}
		sf := spec.SecretFiles[0]
		if sf.Name != secretspkg.ClaudeCodeOAuthSecretName ||
			sf.MountPath != secretspkg.ClaudeCodeOAuthSandboxMountPath ||
			string(sf.Value) != payload {
			t.Fatalf("forfait mount mis-built: name=%q path=%q", sf.Name, sf.MountPath)
		}
	})

	t.Run("unreadable credentials file is a hard error", func(t *testing.T) {
		ctx := secretspkg.WithCredentials(context.Background(), secretspkg.Credentials{
			OAuthCredentialFiles: map[string]string{string(secretspkg.OAuthKindClaudeCode): filepath.Join(t.TempDir(), "gone")},
		})
		var spec sandbox.Spec
		if _, err := addClaudeOAuthSecretFile(ctx, &spec); err == nil {
			t.Fatal("expected a hard error for a missing materialised credentials file")
		}
	})

	t.Run("reserved-name collision with a workflow secret errors", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := secretspkg.WithCredentials(context.Background(), secretspkg.Credentials{
			OAuthCredentialFiles: map[string]string{string(secretspkg.OAuthKindClaudeCode): dir},
		})
		spec := sandbox.Spec{SecretFiles: []sandbox.SecretFileMount{{
			Name: secretspkg.ClaudeCodeOAuthSecretName, MountPath: "/run/iterion/secrets/other",
		}}}
		if _, err := addClaudeOAuthSecretFile(ctx, &spec); err == nil {
			t.Fatal("expected an error on reserved-name collision")
		}
	})
}

// TestSeedClaudeConfigDir pins the seed invocation shape (script + the
// two positional args) and the hard error on a non-zero exit — a run
// that resolved a forfait must fail the sandbox boot loudly rather than
// resurface hours later as an auth error.
func TestSeedClaudeConfigDir(t *testing.T) {
	ok := &fakeSeedRun{}
	if err := seedClaudeConfigDir(context.Background(), ok); err != nil {
		t.Fatalf("seedClaudeConfigDir: %v", err)
	}
	want := []string{"sh", "-c", seedClaudeConfigScript, "sh",
		secretspkg.ClaudeCodeSandboxConfigDir, secretspkg.ClaudeCodeOAuthSandboxMountPath}
	if len(ok.gotCmd) != len(want) {
		t.Fatalf("cmd = %v", ok.gotCmd)
	}
	for i := range want {
		if ok.gotCmd[i] != want[i] {
			t.Fatalf("cmd[%d] = %q, want %q", i, ok.gotCmd[i], want[i])
		}
	}

	bad := &fakeSeedRun{exitCode: 1}
	err := seedClaudeConfigDir(context.Background(), bad)
	if err == nil || !strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("expected a hard exited-1 error, got %v", err)
	}
}
