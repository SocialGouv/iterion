package secrets

import (
	"path"
	"regexp"
	"strings"
)

const SecretFilesMountDir = "/run/iterion/secrets"

// Sandbox delivery of the Claude Code OAuth forfait (ADR-082 Phase 3
// blocker 3). A run whose credentials carry a materialised Claude Code
// .credentials.json ships it into the sandbox as a file secret on the
// ADR-070 channel (per-run k8s Secret / docker temp-dir bind), then the
// runtime seeds a WRITABLE copy the CLI can use as its config dir —
// CLAUDE_CONFIG_DIR must be writable because the claude CLI persists
// session state (and its own token refreshes) under it, while the
// secret mount is read-only by construction.
const (
	// ClaudeCodeOAuthSecretName is the reserved file-secret name for the
	// forfait payload. addClaudeOAuthSecretFile rejects a workflow secret
	// colliding with it.
	ClaudeCodeOAuthSecretName = "claude-code-oauth"

	// ClaudeCodeOAuthSandboxMountPath is where the read-only payload
	// lands inside the sandbox. Kept under SecretFilesMountDir so the
	// k8s driver projects it via the auto-updating directory volume
	// (a custom absolute path would ride subPath, which kubelet never
	// refreshes) and RefreshSecretFile can rotate it mid-run.
	ClaudeCodeOAuthSandboxMountPath = SecretFilesMountDir + "/claude-code-oauth/.credentials.json"

	// ClaudeCodeSandboxConfigDir is the writable in-sandbox
	// CLAUDE_CONFIG_DIR seeded from the mount above. Under /tmp because
	// the pod's volume-mount parents (/run/iterion/*) are kubelet-created
	// root-owned dirs a non-root workload cannot mkdir siblings in.
	ClaudeCodeSandboxConfigDir = "/tmp/iterion-claude-config"

	// ClaudeCodeSandboxCredentialsPath is the seeded credentials file the
	// in-sandbox claude CLI reads (and the runner's mid-run refresher
	// rewrites through the sandbox exec seam).
	ClaudeCodeSandboxCredentialsPath = ClaudeCodeSandboxConfigDir + "/.credentials.json"

	// ClaudeCodeCredentialsFileName is the file the Claude Code forfait
	// blob is materialised as inside a CLAUDE_CONFIG_DIR-shaped directory —
	// including the per-run one the runner builds
	// (Credentials.OAuthDir("claude_code")). Named because several readers
	// now open it: the CLI env builder, the in-process claw factory, and
	// the runner's refresh worker.
	ClaudeCodeCredentialsFileName = ".credentials.json"

	// The same three for the ChatGPT (Codex) forfait. Without them a
	// sandboxed node has no way to see a resolved subscription — the host
	// temp dir does not exist in the container — and silently falls back to
	// whatever ambient key the pod carries.
	CodexOAuthSecretName = "codex-oauth"
	// CodexOAuthSandboxMountPath keeps the payload under SecretFilesMountDir
	// for the same reason as the Claude one: the k8s driver projects that
	// directory volume and can refresh it mid-run.
	CodexOAuthSandboxMountPath = SecretFilesMountDir + "/codex-oauth/auth.json"
	// CodexSandboxConfigDir is the writable in-sandbox CODEX_HOME seeded
	// from the mount above.
	CodexSandboxConfigDir = "/tmp/iterion-codex-config"
	// CodexSandboxAuthPath is the seeded auth.json the in-sandbox reader
	// (claw's registry, or the codex CLI) opens.
	CodexSandboxAuthPath = CodexSandboxConfigDir + "/auth.json"
)

var secretFileNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// SanitizeFileName reduces a secret name to a safe basename for a secret
// file (letters, digits, `_`, `.`, `-`). It is the shared rule behind
// DefaultFileMountPath (sandbox mount) and the host-side materialisation
// used by non-sandbox runs, so the same secret lands under the same
// filename on either path.
func SanitizeFileName(name string) string {
	clean := secretFileNameSanitizer.ReplaceAllString(name, "_")
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		clean = "secret"
	}
	return clean
}

// DefaultFileMountPath returns the stable in-sandbox file path for a
// workflow secret mounted as a file. The path is deterministic so prompts
// can reference it before the sandbox container is started.
func DefaultFileMountPath(name string) string {
	return SecretFilesMountDir + "/" + SanitizeFileName(name)
}

func ResolveFileMountPath(name, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return DefaultFileMountPath(name)
}

// RelativeToSecretFilesMountDir returns mountPath relative to the default
// file-secret directory when mountPath lives directly under it. The caller is
// expected to validate mountPath as a clean absolute file path first.
func RelativeToSecretFilesMountDir(mountPath string) (string, bool) {
	clean := path.Clean(mountPath)
	if clean != mountPath {
		return "", false
	}
	prefix := SecretFilesMountDir + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(clean, prefix)
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", false
	}
	return rel, true
}
