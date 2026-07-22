package runtime

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func workflowHasFileSecrets(wf *ir.Workflow) bool {
	if wf == nil {
		return false
	}
	for _, s := range wf.Secrets {
		if s.IsFile() {
			return true
		}
	}
	return false
}

func addSecretFileMounts(ctx context.Context, spec *sandbox.Spec, wf *ir.Workflow, vars map[string]any) error {
	if spec == nil || wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	names := make([]string, 0, len(wf.Secrets))
	for name := range wf.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	seenMountPaths := map[string]string{}
	for _, name := range names {
		s := wf.Secrets[name]
		if !s.IsFile() {
			continue
		}
		value := ""
		if strings.TrimSpace(s.Value) != "" {
			value = resolveRuntimeSecretValue(s.Value, vars)
		} else {
			value = creds.GenericSecret(name)
		}
		if value == "" {
			if s.Optional {
				// Optional file secret with no resolved value: skip the
				// mount silently so a bot that only needs it on some runs
				// (e.g. a forge token when posting a review) still runs
				// without it. The agent simply won't find the file.
				continue
			}
			return fmt.Errorf("runtime: sandbox: file secret %q has no resolved value; set secrets.%s.value or configure a stored secret named %q (or mark it optional: true)", name, name, name)
		}

		mountPath := secrets.ResolveFileMountPath(name, s.MountPath)
		if !strings.HasPrefix(mountPath, "/") {
			return fmt.Errorf("runtime: sandbox: file secret %q mount_path must be absolute: %q", name, mountPath)
		}
		if strings.ContainsAny(mountPath, "\n\r\x00") {
			return fmt.Errorf("runtime: sandbox: file secret %q mount_path contains a control character", name)
		}
		cleanMountPath := path.Clean(mountPath)
		if cleanMountPath != mountPath || cleanMountPath == "/" {
			return fmt.Errorf("runtime: sandbox: file secret %q mount_path must be a clean absolute file path: %q", name, mountPath)
		}
		if prev := seenMountPaths[cleanMountPath]; prev != "" {
			return fmt.Errorf("runtime: sandbox: file secrets %q and %q resolve to the same mount_path %q", prev, name, cleanMountPath)
		}
		seenMountPaths[cleanMountPath] = name

		if s.Env != "" {
			if spec.Env == nil {
				spec.Env = map[string]string{}
			}
			spec.Env[s.Env] = mountPath
		}
		spec.SecretFiles = append(spec.SecretFiles, sandbox.SecretFileMount{
			Name:      name,
			MountPath: mountPath,
			Env:       s.Env,
			Value:     []byte(value),
		})
	}
	return nil
}

// addClaudeOAuthSecretFile ships the run's materialised Claude Code
// OAuth forfait (.credentials.json) into the sandbox as a file secret on
// the ADR-070 channel (ADR-082 Phase 3 blocker 3). Without it, in-pod
// claude auth hangs on the single exec-env CLAUDE_CODE_OAUTH_TOKEN
// value: the host CLAUDE_CONFIG_DIR temp dir does not exist inside a
// kubernetes pod, so a long-lived CLI session has no credentials file to
// fall back to (and nothing to self-refresh from) once that token
// expires.
//
// Returns true when the mount was added. Silently false when the run
// carries no materialised forfait (the normal local case). A present
// but unreadable credentials file is a hard error — the run WOULD have
// authenticated via the forfait, so degrading silently to env-only auth
// would reintroduce the exact fragility this closes.
//
// Callers must only invoke this for a real (non-noop) driver: the noop
// path rejects any SecretFiles.
func addClaudeOAuthSecretFile(ctx context.Context, spec *sandbox.Spec) (bool, error) {
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return false, nil
	}
	dir := creds.OAuthDir(string(secrets.OAuthKindClaudeCode))
	if dir == "" {
		return false, nil
	}
	payload, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return false, fmt.Errorf("read materialised claude forfait credentials: %w", err)
	}
	for _, sf := range spec.SecretFiles {
		if sf.Name == secrets.ClaudeCodeOAuthSecretName {
			return false, fmt.Errorf("file secret name %q is reserved for the Claude forfait delivery; rename the workflow secret", sf.Name)
		}
		if sf.MountPath == secrets.ClaudeCodeOAuthSandboxMountPath {
			return false, fmt.Errorf("file secret %q mount_path %q collides with the Claude forfait delivery path", sf.Name, sf.MountPath)
		}
	}
	spec.SecretFiles = append(spec.SecretFiles, sandbox.SecretFileMount{
		Name:      secrets.ClaudeCodeOAuthSecretName,
		MountPath: secrets.ClaudeCodeOAuthSandboxMountPath,
		Value:     payload,
	})
	return true, nil
}

// seedClaudeConfigScript copies the read-only forfait mount into a
// WRITABLE CLAUDE_CONFIG_DIR ($1 = config dir, $2 = mounted payload).
// The claude CLI persists session state — and its own token refreshes —
// under its config dir, so pointing it at the read-only secret mount
// would break it; the seeded copy is owner-only (0700 dir / 0600 file)
// and rewritten mid-run by the runner's forfait refresher through the
// same exec seam.
const seedClaudeConfigScript = `set -e
umask 077
mkdir -p "$1"
cp "$2" "$1/.credentials.json"
chmod 600 "$1/.credentials.json"`

// seedClaudeConfigDir runs [seedClaudeConfigScript] inside the freshly
// started sandbox. Hard error on failure: the run resolved a forfait
// credential, so a half-delivered config dir must fail the boot loudly
// rather than surface hours later as an auth error mid-workflow.
func seedClaudeConfigDir(ctx context.Context, run sandbox.Run) error {
	res, err := run.Exec(ctx, []string{
		"sh", "-c", seedClaudeConfigScript, "sh",
		secrets.ClaudeCodeSandboxConfigDir, secrets.ClaudeCodeOAuthSandboxMountPath,
	}, sandbox.ExecOpts{})
	if err != nil {
		return fmt.Errorf("runtime: sandbox: seed claude config dir: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("runtime: sandbox: seed claude config dir: exited %d: %s",
			res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

func resolveRuntimeSecretValue(expr string, vars map[string]any) string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "{{") && strings.HasSuffix(expr, "}}") {
		inner := strings.TrimSpace(expr[2 : len(expr)-2])
		if rest, ok := strings.CutPrefix(inner, "vars."); ok {
			key := strings.TrimSpace(rest)
			if vars == nil {
				return ""
			}
			if v, ok := vars[key]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
	}
	return ir.ExpandEnvWithDefault(expr)
}
