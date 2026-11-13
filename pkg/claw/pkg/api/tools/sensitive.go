package tools

import (
	intl "github.com/SocialGouv/iterion/pkg/claw/internal/tools"
)

// CredentialReadGuard decides whether a read target is a credential or
// secret location a host's read gate refuses (iterion#2868). The
// exclusion follows the paths a credential is actually read from — real
// layouts at any depth under a read root, and the locations derived from
// the process's own environment (KUBECONFIG, NPM_CONFIG_USERCONFIG,
// CODEX_HOME, AWS_SHARED_CREDENTIALS_FILE, DOCKER_CONFIG, GH_CONFIG_DIR,
// CLAUDE_CONFIG_DIR, HOME) — rather than a frozen table of names: a
// fixture or template carrying a credential-looking name outside those
// locations stays readable. See the internal type for the full contract.
type CredentialReadGuard = intl.CredentialReadGuard

// NewCredentialReadGuard derives the guard from the process environment.
// A nil env reads os.Getenv.
func NewCredentialReadGuard(env func(string) string) *CredentialReadGuard {
	return intl.NewCredentialReadGuard(env)
}
