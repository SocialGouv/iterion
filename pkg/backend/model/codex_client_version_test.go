package model

import (
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func TestNewerCodexVersion(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"0.139.0", "0.144.6", "0.144.6"},
		{"0.153.4", "0.144.6", "0.153.4"},
		{"", "0.144.6", "0.144.6"},
		{"garbage", "0.144.6", "0.144.6"},
		{"0.144.6", "", "0.144.6"},
		{"0.144", "0.144.0", "0.144"},
		{"v0.145.0-beta.1", "0.144.6", "v0.145.0-beta.1"},
		{"0.145.0-beta.1", "0.145.0", "0.145.0"},
		{"0.145.0", "0.145.0-rc.2", "0.145.0"},
		{"0.145.0-beta.1", "0.145.0-beta.2", "0.145.0-beta.1"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := newerCodexVersion(c.a, c.b); got != c.want {
			t.Errorf("newerCodexVersion(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// A stale codex binary on the host must never drag the identity below the
// release claw itself presents; an operator override still passes as-is.
func TestCodexCLIVersion_HostNeverBelowBaked(t *testing.T) {
	codexVersionOnce.Do(func() {})
	orig := codexVersionCached
	t.Cleanup(func() { codexVersionCached = orig })

	t.Setenv("ITERION_CODEX_VERSION", "")
	codexVersionCached = "0.139.0"
	if got := codexCLIVersion(); got != api.ChatGPTClientVersion {
		t.Errorf("stale host binary: got %q, want the baked %q", got, api.ChatGPTClientVersion)
	}
	codexVersionCached = "0.199.0"
	if got := codexCLIVersion(); got != "0.199.0" {
		t.Errorf("newer host binary: got %q, want it forwarded", got)
	}
	codexVersionCached = ""
	if got := codexCLIVersion(); got != api.ChatGPTClientVersion {
		t.Errorf("no host binary: got %q, want the baked %q", got, api.ChatGPTClientVersion)
	}

	t.Setenv("ITERION_CODEX_VERSION", "0.100.0")
	if got := codexCLIVersion(); got != "0.100.0" {
		t.Errorf("operator override: got %q, want it sent as-is", got)
	}
}

// Inside a sandbox the launcher forwards its own probe as
// ITERION_CODEX_HOST_VERSION; the runner keeps the newest of that probe, its
// own (absent) binary and its baked release — never below the baked one.
func TestCodexCLIVersion_HostProbeIsAProbeNotADecision(t *testing.T) {
	codexVersionOnce.Do(func() {})
	orig := codexVersionCached
	t.Cleanup(func() { codexVersionCached = orig })
	t.Setenv("ITERION_CODEX_VERSION", "")
	codexVersionCached = ""

	t.Setenv(codexHostVersionEnv, "0.139.0")
	if got := codexCLIVersion(); got != api.ChatGPTClientVersion {
		t.Errorf("stale host probe: got %q, want the baked %q", got, api.ChatGPTClientVersion)
	}
	t.Setenv(codexHostVersionEnv, "0.199.0")
	if got := codexCLIVersion(); got != "0.199.0" {
		t.Errorf("newer host probe: got %q, want it used", got)
	}
}
