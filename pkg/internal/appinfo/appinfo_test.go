package appinfo

import "testing"

// SandboxImageTag treats any leading "v" as a release marker: release
// builds pull the version-pinned sandbox image, everything else tracks
// the rolling :edge. This pins the contract the image build must feed —
// a malformed injected Version like "vmain" or "vv0.32.0" (the
// github.ref_name bug this PR fixes) silently becomes a phantom image
// tag instead of falling back to edge, so the build must only ever
// inject "v<semver>".
func TestSandboxImageTag(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	cases := []struct {
		version string
		want    string
	}{
		{"v0.32.0", "v0.32.0"},   // release build → version-pinned tag
		{"dev", "edge"},          // default (no ldflags) → rolling edge
		{"", "edge"},             // empty → rolling edge
		{"0.32.0", "edge"},       // bare semver (no v) → rolling edge
		{"  v1.2.3  ", "v1.2.3"}, // ldflags whitespace is trimmed
	}
	for _, c := range cases {
		Version = c.version
		if got := SandboxImageTag(); got != c.want {
			t.Errorf("SandboxImageTag() with Version=%q = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestFullVersion(t *testing.T) {
	origV, origC, origM := Version, Commit, Modified
	t.Cleanup(func() { Version, Commit, Modified = origV, origC, origM })

	Version, Commit, Modified = "v0.32.0", "abcdef123456789", false
	if got, want := FullVersion(), "v0.32.0+abcdef123456"; got != want {
		t.Errorf("FullVersion() = %q, want %q (commit truncated to 12)", got, want)
	}
	Version, Commit, Modified = "", "", false
	if got := FullVersion(); got != "dev" {
		t.Errorf("FullVersion() with empty Version = %q, want \"dev\"", got)
	}
	Version, Commit, Modified = "dev", "abcdef123456789", true
	if got, want := FullVersion(), "dev+abcdef123456-dirty"; got != want {
		t.Errorf("FullVersion() for modified build = %q, want %q", got, want)
	}
	Version, Commit, Modified = "", "", true
	if got, want := FullVersion(), "dev-dirty"; got != want {
		t.Errorf("FullVersion() for modified build without commit = %q, want %q", got, want)
	}
}
