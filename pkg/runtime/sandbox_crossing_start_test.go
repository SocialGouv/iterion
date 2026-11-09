package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WITNESSES (ADR-162 decision 7, U5) on the START itself, through the real
// resolveAndStartSandbox: a start that cannot arm the placeholder
// substitution — TLS inspection off (the operator's hatch) or a driver
// without it — refuses the untrusted run TYPED rather than hand its
// container placeholders that resolve nowhere; and an untrusted run's
// sandbox spec carries no forfait mount, while a trusted run's keeps today's
// delivery.
//
// Mutants: the arming gate's conditions (`!sandboxTLSInspectEnabled() ||
// !realDriver || !caps.SupportsTLSInspection`) losing any term → the hatch
// and driver rows; the `p.Trust.Trusted()` guard on the forfait delivery →
// the mount row (fork mounts) and its trusted control (trusted stops
// mounting).
func TestAStartThatCannotArmTheCrossingRefusesTheForkRun(t *testing.T) {
	clearCredentialEnv(t)
	key := "sk-ant-crossing-start-witness"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: key},
	})
	wf := credentialWorkflow("review", "claude_code", "none")

	t.Run("TLS inspection off — the operator's hatch", func(t *testing.T) {
		t.Setenv("ITERION_SANDBOX_TLS_INSPECT", "off")
		err := credentialAdmissionRunCtx(t, ctx, wf, store.RunTrustFork, nil)
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCrossingRefused {
			t.Fatalf("fork run on a start with inspection off = %v, want the typed crossing refusal", err)
		}
		if errors.Is(err, errCanaryStop) {
			t.Fatal("the launch went on: the container would hold placeholders nothing resolves")
		}
		if !strings.Contains(rerr.Message, "TLS inspection") && !strings.Contains(rerr.Hint, "TLS inspection") {
			t.Fatalf("the refusal must say the start cannot arm the substitution: %q / %q", rerr.Message, rerr.Hint)
		}
	})

	t.Run("a driver without TLS inspection", func(t *testing.T) {
		err := crossingStart(t, ctx, wf, store.RunTrustFork, &specCapturingDriver{hostBinds: true})
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCrossingRefused {
			t.Fatalf("fork run on a driver without inspection = %v, want the typed crossing refusal", err)
		}
	})

	t.Run("controls: a start that can arm it admits, and a trusted run never asks", func(t *testing.T) {
		if err := credentialAdmissionRunCtx(t, ctx, wf, store.RunTrustFork, nil); !errors.Is(err, errCanaryStop) {
			t.Fatalf("fork run on a TLS-capable start = %v, want admission (canary)", err)
		}
		if err := crossingStart(t, ctx, wf, store.RunTrustDefault, &specCapturingDriver{hostBinds: true}); !errors.Is(err, errCanaryStop) {
			t.Fatalf("trusted run on the same driver = %v, want admission (canary) — the crossing is the fork lane's question", err)
		}
	})
}

// TestTheForkLanesSandboxShipsNoForfaitMount: the forfaits are OAuth
// credentials — a file and a refresh flow — the egress proxy cannot hold,
// so an untrusted run's spec mounts neither kind and exports neither config
// dir; a trusted run's spec keeps today's delivery, file and dir both.
func TestTheForkLanesSandboxShipsNoForfaitMount(t *testing.T) {
	clearCredentialEnv(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-x","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-mount-witness"},
		OAuthCredentialFiles: map[string]string{
			string(secrets.OAuthKindClaudeCode): claudeDir,
			string(secrets.OAuthKindCodex):      codexDir,
		},
	})
	wf := credentialWorkflow("review", "claude_code", "none")

	for _, row := range []struct {
		name    string
		trust   store.RunTrust
		mounted bool
	}{
		{"fork lane", store.RunTrustFork, false},
		{"trusted run (the control)", store.RunTrustDefault, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			d := &tlsCapableDriver{specCapturingDriver{hostBinds: true}}
			err := crossingStart(t, ctx, wf, row.trust, d)
			if !errors.Is(err, errCanaryStop) {
				t.Fatalf("resolveAndStartSandbox = %v, want the canary (the run's key funds the leg)", err)
			}
			for _, name := range []string{secrets.ClaudeCodeOAuthSecretName, secrets.CodexOAuthSecretName} {
				if has := specMountsSecret(d.captured, name); has != row.mounted {
					t.Errorf("spec mounts the %s forfait = %v, want %v", name, has, row.mounted)
				}
			}
			for name, want := range map[string]string{
				"CLAUDE_CONFIG_DIR": secrets.ClaudeCodeSandboxConfigDir,
				"CODEX_HOME":        secrets.CodexSandboxConfigDir,
			} {
				if got := d.captured.Env[name]; (got == want) != row.mounted {
					t.Errorf("spec env %s = %q, want the config dir %q exactly when mounted", name, got, want)
				}
			}
		})
	}
}

// crossingStart drives resolveAndStartSandbox on the given driver and trust,
// the same shape credentialAdmissionDetecting pins, returning its error.
func crossingStart(t *testing.T, ctx context.Context, wf *ir.Workflow, trust store.RunTrust, d sandbox.Driver) error {
	t.Helper()
	t.Setenv("ITERION_MODE", "local")
	_, err := resolveAndStartSandbox(ctx, SandboxParams{
		Workflow:      wf,
		RunID:         "run-crossing-start",
		Trust:         trust,
		WorkspacePath: t.TempDir(),
		RepoRoot:      t.TempDir(),
		DefaultImage:  "registry.example.test/operator-sandbox:latest",
		Drivers:       map[string]sandbox.DriverConstructor{"docker": func() (sandbox.Driver, error) { return d, nil }},
		Logger:        iterlog.Nop(),
		EmitEvent:     func(store.EventType, map[string]any) error { return nil },
	})
	return err
}

func specMountsSecret(spec sandbox.Spec, name string) bool {
	for _, sf := range spec.SecretFiles {
		if sf.Name == name {
			return true
		}
	}
	return false
}
