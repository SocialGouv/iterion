package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// `iterion validate` must surface the same requirement the push guard and the
// runner enforce — locally, before the bundle is anywhere near a deployment.

func requiresManifest(t *testing.T, iterion string) *bundle.Manifest {
	t.Helper()
	src := "name: needy\nversion: 1.6.0\n"
	if iterion != "" {
		src += "requires:\n  iterion: \"" + iterion + "\"\n"
	}
	m, err := bundle.DecodeManifest([]byte(src), "t")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func findDiag(diags []Diag, code Code) (Diag, bool) {
	for _, d := range diags {
		if d.Code == code {
			return d, true
		}
	}
	return Diag{}, false
}

func TestEngineRequirementUnmetIsAnError(t *testing.T) {
	diags := CheckConsistency(Input{Manifest: requiresManifest(t, ">= 3.112.14"), EngineBuild: "v3.112.7+abc"})
	d, ok := findDiag(diags, DiagEngineRequirementUnmet)
	if !ok {
		t.Fatalf("no C250 for a bundle this build cannot run: %+v", diags)
	}
	if d.Severity != SeverityError {
		t.Error("C250 must be an error — the run cannot succeed on this build")
	}
	if d.Field != "requires.iterion" {
		t.Errorf("field = %q, want requires.iterion (the manifest is the attribution surface)", d.Field)
	}
	if !strings.Contains(d.Message, "3.112.14") || !strings.Contains(d.Message, "3.112.7") {
		t.Errorf("message %q must name both the floor and the running build", d.Message)
	}
}

func TestEngineRequirementUncheckableIsAWarning(t *testing.T) {
	diags := CheckConsistency(Input{Manifest: requiresManifest(t, ">= 3.112.14"), EngineBuild: "dev"})
	d, ok := findDiag(diags, DiagEngineRequirementUnchecked)
	if !ok {
		t.Fatalf("no C251 on a build with no orderable version: %+v", diags)
	}
	if d.Severity != SeverityWarning {
		t.Error("C251 must WARN, not reject: refusing every dev build would make `iterion validate` unusable in the repo that authors the bundles")
	}
	if _, unmet := findDiag(diags, DiagEngineRequirementUnmet); unmet {
		t.Error("an undecidable comparison must not also claim the requirement is unmet")
	}
}

func TestEngineRequirementSatisfiedIsQuiet(t *testing.T) {
	for _, tc := range []struct{ name, requires, build string }{
		{"met exactly", ">= 3.112.14", "v3.112.14"},
		{"met with room", ">= 3.112.14", "v3.116.4+deadbeef"},
		{"no requirement", "", "v3.112.7"},
		{"no build supplied", ">= 3.112.14", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := CheckConsistency(Input{Manifest: requiresManifest(t, tc.requires), EngineBuild: tc.build})
			for _, code := range []Code{DiagEngineRequirementUnmet, DiagEngineRequirementUnchecked} {
				if d, found := findDiag(diags, code); found {
					t.Errorf("unexpected %s: %s", code, d.Message)
				}
			}
		})
	}
}
