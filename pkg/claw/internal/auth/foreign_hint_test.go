package auth

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// A foreign-provider hint must not send an operator onto the Anthropic wire
// for a key this package already resolves to a native provider of its own.
// Held to ResolveCredentials rather than to a list: for each hinted variable,
// set it ALONE, ask the resolver which provider it names, and read the hint
// for that variable. A native answer other than "anthropic" means the key
// needs no redirect — and a hint telling the operator to point
// ANTHROPIC_BASE_URL at that vendor would retarget every other
// Anthropic-wire caller sharing the environment.
func TestForeignProviderHints_DoNotRedirectTheAnthropicWireForANativeKey(t *testing.T) {
	envs := map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true}
	for _, fp := range api.ForeignProviderEnvVars {
		envs[fp.EnvVar] = true
	}
	for _, fp := range api.ForeignProviderEnvVars {
		t.Run(fp.EnvVar, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // no credential store
			for k := range envs {
				t.Setenv(k, "")
			}
			t.Setenv(fp.EnvVar, "key-for-"+fp.EnvVar)

			provider, _, _, err := ResolveCredentials()
			if err != nil || provider == "" || provider == "anthropic" {
				return // not resolved natively: nothing to contradict
			}
			if strings.Contains(fp.Hint, "ANTHROPIC_BASE_URL=") {
				t.Errorf("%s alone resolves to the native %q provider, but its hint tells the operator to redirect the Anthropic wire: %q",
					fp.EnvVar, provider, fp.Hint)
			}
		})
	}
}
