package ir

import (
	"strings"
	"testing"
)

// The class form of `sandbox: { image: … }` (decision (a) of #2632): a bare
// word naming one of the stock image classes resolves at launch to the
// published variant image; the IR keeps the class NAME, since the tag follows
// the running binary and is unknowable at compile time.

func compileSandboxImage(t *testing.T, imageLine string) (*Workflow, []Diagnostic) {
	t.Helper()
	return compileSource(t,
		"agent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  sandbox:\n    "+imageLine+"\n  a -> done\n")
}

func sandboxImageDiags(diags []Diagnostic, code DiagCode) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

func TestSandboxImageClassKnownNamesCompileClean(t *testing.T) {
	for _, class := range []string{"slim", "full", "sec", "browser", "rgaa"} {
		wf, diags := compileSandboxImage(t, "image: "+class)
		if errs := errorsOnly(diags); len(errs) > 0 {
			t.Fatalf("image: %s: compile errors: %v", class, errs)
		}
		if got := wf.Sandbox.Image; got != class {
			t.Errorf("image: %s: IR carries %q — the class name is the value, the launch resolves it", class, got)
		}
		if hits := sandboxImageDiags(diags, DiagSandboxImageClassUnknown); len(hits) > 0 {
			t.Errorf("image: %s: C325 fired on a known class: %v", class, hits)
		}
	}
}

func TestSandboxImageUnknownBareNameWarnsAndKeepsTheValue(t *testing.T) {
	wf, diags := compileSandboxImage(t, "image: bogus")
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("an unknown bare image name is a warning, never a refusal: %v", errs)
	}
	hits := sandboxImageDiags(diags, DiagSandboxImageClassUnknown)
	if len(hits) != 1 {
		t.Fatalf("image: bogus: diagnostics = %v, want exactly one C325", diags)
	}
	for _, want := range []string{"bogus", "slim", "browser", "rgaa"} {
		if !strings.Contains(hits[0].Message, want) {
			t.Errorf("C325 message %q does not name %q", hits[0].Message, want)
		}
	}
	if got := wf.Sandbox.Image; got != "bogus" {
		t.Errorf("IR carries %q, want the author's %q — a warning must not cost the declaration", got, "bogus")
	}
}

func TestSandboxImageFreeFormAndIndirectValuesDoNotWarn(t *testing.T) {
	for _, line := range []string{
		`image: "ghcr.io/an-operator/custom:1"`, // a full reference is the escape hatch
		`image: "alpine:3.20"`,                  // a tagged bare name is a docker.io reference
		`image: "${SANDBOX_IMAGE}"`,             // an indirection the launcher resolves
		`image: "repo/img@sha256:abab"`,         // digest-pinned
	} {
		_, diags := compileSandboxImage(t, line)
		if errs := errorsOnly(diags); len(errs) > 0 {
			t.Fatalf("%s: compile errors: %v", line, errs)
		}
		if hits := sandboxImageDiags(diags, DiagSandboxImageClassUnknown); len(hits) > 0 {
			t.Errorf("%s: C325 fired on a value that is not a bare class candidate: %v", line, hits)
		}
	}
}
