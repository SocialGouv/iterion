package ir

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox/imageclass"
)

// warnUnknownSandboxImageClass emits C325 when a sandbox block's `image:` is
// a bare word that names no stock image class (#2632). The class form
// (`image: rgaa`) resolves at launch to the published variant image; every
// other value keeps its meaning, so the check is a WARNING, never an error:
//
//   - a full reference (`ghcr.io/…/img:tag`, a digest pin) is the operator's
//     own image — the escape hatch the closed vocabulary exists alongside;
//   - an indirection (`${VAR}`, `{{vars.x}}`) is the launcher's to resolve,
//     and the compiler does not guess;
//   - a bare word that is no class (`alpine`, `my-local-image`) passes
//     through verbatim — it may be a locally built image, and refusing it
//     would close a working seam. The warning names both readings so the
//     author picks deliberately rather than discovering at pull time.
//
// The IR keeps the author's value either way: the class resolves where the
// running binary's tag is known (pkg/runtime's resolveSandboxImageClass).
func (c *compiler) warnUnknownSandboxImageClass(image, scope, name string) {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" || !imageclass.LooksLikeClassName(trimmed) || imageclass.Known(trimmed) {
		return
	}
	c.warnfAtProperty(DiagSandboxImageClassUnknown, scope, name, "sandbox",
		"%s %q declares sandbox.image %q: a bare word that names no stock image class (%s) — at launch it is passed to the registry VERBATIM (a local or docker.io/library image), not resolved to an image iterion publishes; name a class for a published variant, or a full reference (registry/repo:tag) for your own image",
		scope, name, trimmed, strings.Join(imageclass.Classes(), ", "))
}
