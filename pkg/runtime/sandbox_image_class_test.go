package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestAClassNamedSandboxImageResolvesToTheStockVariant is decision (a) of
// #2632 at the seam where it acts: a bot that declares
// `sandbox: { image: rgaa }` names a CAPABILITY CLASS, and the run's sandbox
// starts from the published variant image of that class — version-pinned to
// the running iterion exactly like the default slim image, with the same
// `:latest` fallback a between-releases binary needs.
func TestAClassNamedSandboxImageResolvesToTheStockVariant(t *testing.T) {
	const namespace = "ghcr.io/socialgouv/iterion-sandbox-"
	for _, class := range []string{"slim", "full", "sec", "browser", "rgaa"} {
		wf := &ir.Workflow{
			Name:    "wf",
			Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeInline), Image: class},
		}
		res, err := resolveSandboxSpecWithFallback(wf, t.TempDir(), "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("class %q: resolveSandboxSpecWithFallback: %v", class, err)
		}
		want := namespace + class + ":" + appinfo.SandboxImageTag()
		if res.spec.Image != want {
			t.Errorf("class %q: spec.Image = %q, want %q — the class must resolve to the published variant, pinned to the running iterion", class, res.spec.Image, want)
		}
		if wantFallback := namespace + class + ":latest"; res.spec.ImageFallback != wantFallback {
			t.Errorf("class %q: spec.ImageFallback = %q, want %q — a version-pinned stock image carries the same registry fallback the built-in default does", class, res.spec.ImageFallback, wantFallback)
		}
	}
}

// TestAFreeFormSandboxImageIsNeverReadAsAClass is the escape hatch holding in
// both directions: a full reference is the operator's own image and passes
// through byte-identical, and a bare word that is no known class does too —
// it may be a locally built image, and re-pointing it at a registry would be
// the silent replacement philosophy #1 refuses. The compiler's C325 warning
// is what tells the two bare-word readings apart; the runtime honours the
// text as written.
func TestAFreeFormSandboxImageIsNeverReadAsAClass(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/an-operator/iterion-sandbox-rgaa:with-extra", // their namespace, their contents
		"registry.example.invalid:5000/team/sandbox:1",        // a registry port is not a tag
		"alpine:3.20",      // a tagged bare name is a docker.io reference
		"my-local-image",   // unknown class: a local image, verbatim
		"${SANDBOX_IMAGE}", // an indirection the launcher resolves, not a class
		"ghcr.io/socialgouv/iterion-sandbox-browser@sha256:" + strings.Repeat("ab", 32),
	} {
		wf := &ir.Workflow{
			Name:    "wf",
			Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeInline), Image: image},
		}
		res, err := resolveSandboxSpecWithFallback(wf, t.TempDir(), "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("image %q: resolveSandboxSpecWithFallback: %v", image, err)
		}
		if res.spec.Image != image {
			t.Errorf("image %q: spec.Image = %q, want it verbatim — only a KNOWN class name resolves", image, res.spec.Image)
		}
		if res.spec.ImageFallback != "" {
			t.Errorf("image %q: spec.ImageFallback = %q, want none — the fallback is reserved to images iterion itself named", image, res.spec.ImageFallback)
		}
	}
}
