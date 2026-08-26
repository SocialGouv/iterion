package runtime

import (
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// EnvSandboxDefaultImage is the env var override for the implicit
// sandbox image used by `sandbox: auto` when no
// .devcontainer/devcontainer.json is found in the workspace.
const EnvSandboxDefaultImage = "ITERION_SANDBOX_DEFAULT_IMAGE"

// builtInSandboxImageRepo is the GHCR repository iterion publishes
// the slim sandbox image to. Fully-qualified image refs are built as
// "<repo>:<tag>" where tag tracks the iterion binary version.
const builtInSandboxImageRepo = "ghcr.io/socialgouv/iterion-sandbox-slim"

// resolveDefaultSandboxImage returns the image ref to use as fallback
// when `sandbox: auto` is active but no .devcontainer/devcontainer.json
// exists. Precedence (highest first):
//
//  1. flagOverride argument (--sandbox-default-image flag)
//  2. ITERION_SANDBOX_DEFAULT_IMAGE env var
//  3. built-in pinned to the iterion binary version
//
// Always returns a non-empty ref so auto-mode without a devcontainer
// can proceed by default. Operators who want explicit no-fallback
// behaviour can declare an inline sandbox block in the workflow.
func resolveDefaultSandboxImage(flagOverride string) string {
	ref, _ := resolveDefaultSandboxImageWithFallback(flagOverride)
	return ref
}

// resolveDefaultSandboxImageWithFallback additionally reports a fallback
// ref for the case where the chosen image does not exist at the
// registry. A binary built from a commit whose release never published
// a sandbox image (or built between releases) pins a tag nobody pushed:
// the pull then fails with a raw "manifest unknown" and the run dies at
// startup, before any node.
//
// The fallback is offered ONLY for the version-pinned built-in. An
// image the operator named — flag or env — is honoured exactly: a
// sandbox that silently runs a different image than the one asked for
// is worse than one that refuses to start.
func resolveDefaultSandboxImageWithFallback(flagOverride string) (ref, fallback string) {
	if v := strings.TrimSpace(flagOverride); v != "" {
		return v, ""
	}
	if v := strings.TrimSpace(os.Getenv(EnvSandboxDefaultImage)); v != "" {
		return v, ""
	}
	return builtInSandboxImageRepo + ":" + appinfo.SandboxImageTag(), builtInSandboxImageRepo + ":latest"
}
