package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Each attempt forwards exactly ITS provider's run-held key: the chain sets
// task.Model/ProviderHint per element, and the env loop narrows to that. The
// chain is driven one dispatch per element, in both orders — the verdict
// must not depend on which element goes first.
func TestDispatchChain_ForwardsOnlyTheActiveElementRunKey(t *testing.T) {
	zaiEl := chainElement{Provider: "zai", Model: "zai/glm-5.2"}
	anEl := chainElement{Provider: "anthropic", Model: "anthropic/claude-haiku-4-5"}
	for _, order := range [][]chainElement{
		{zaiEl, anEl},
		{anEl, zaiEl},
	} {
		t.Setenv("ZAI_API_KEY", "platform-zai")
		t.Setenv("ANTHROPIC_API_KEY", "platform-an")
		runKeys := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{
				secrets.ProviderZAI:       "tenant-zai",
				secrets.ProviderAnthropic: "tenant-an",
			},
		})
		box := newDeliverySandbox(t)
		b := newTestClawBackend()
		for i, el := range order {
			// The deliverySandbox is a stub runner: it records the spawn env
			// and exits without speaking the result protocol, so the
			// dispatch's error is the stub's. The subject is the envs.
			_, _ = b.executeViaSandboxRunner(runKeys, delegate.Task{
				NodeID: "n", Model: el.Model, ProviderHint: el.Provider,
				WorkDir: t.TempDir(), Sandbox: box,
			})
			env := box.runEnvs[i]
			want, ambient := "tenant-zai", "platform-an"
			if el.Provider == "anthropic" {
				want, ambient = "tenant-an", "platform-zai"
			}
			zai, an := env["ZAI_API_KEY"], env["ANTHROPIC_API_KEY"]
			if el.Provider == "anthropic" {
				if an != want || zai != ambient {
					t.Errorf("order %v attempt %d (%s): zai=%q an=%q, want the run's key %q and the ambient %q", order, i, el.Provider, zai, an, want, ambient)
				}
			} else if zai != want || an != ambient {
				t.Errorf("order %v attempt %d (%s): zai=%q an=%q, want the run's key %q and the ambient %q", order, i, el.Provider, zai, an, want, ambient)
			}
		}
	}
}
