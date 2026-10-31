package runtime

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/sandbox/imageclass"
)

// resolveSandboxImageClass resolves a workflow-declared `sandbox: { image: … }`
// that names a stock image CLASS (`image: rgaa`, #2632) to the published
// variant's fully-qualified reference: "<namespace>/iterion-sandbox-<class>"
// — the same repository family the default `sandbox: auto` resolves to
// ([builtInSandboxImageRepo], so the two cannot separate) — pinned to the
// running iterion's tag, with the same `:latest` registry fallback
// [resolveDefaultSandboxImageWithFallback] offers the built-in: a binary
// built between releases pins a tag nobody pushed, and the class is
// iterion's own image, so the fallback is as honest here as there.
//
// Everything else passes through VERBATIM with no fallback, the value the
// author wrote being honoured exactly:
//
//   - a full reference (carries '/', ':' or '@') is the operator's own
//     image — the escape hatch this vocabulary exists alongside;
//   - a bare word that is NO known class may be a locally built image —
//     the compiler's C325 already said it is no class; re-pointing it at a
//     registry here would be the silent replacement philosophy #1 refuses;
//   - an indirection ('$…', '{{…}}') is the launcher's to resolve.
func resolveSandboxImageClass(image string) (ref, fallback string) {
	name := strings.TrimSpace(image)
	if !imageclass.LooksLikeClassName(name) || !imageclass.Known(name) {
		return image, ""
	}
	prefix := strings.TrimSuffix(builtInSandboxImageRepo, "slim")
	if prefix == builtInSandboxImageRepo {
		// The built-in stopped being the slim variant: the family prefix can
		// no longer be derived, so pass the name through rather than resolve
		// it against a guess.
		return image, ""
	}
	return prefix + name + ":" + appinfo.SandboxImageTag(), prefix + name + ":latest"
}
