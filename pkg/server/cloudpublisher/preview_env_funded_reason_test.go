package cloudpublisher

// Adversarial proof (rva round 1, surface reviewer) — isolated copy only.
// Class fixed by 31cdebc12: the pool block claimed "credentials_present"
// (the empty-bundle gate's code) with no candidate present. Residual site:
// the env-funded run, whose walk never runs.

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

func TestPreviewReason_EnvFundedPreviewDoesNotClaimCredentialsPresent(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	p := &Publisher{
		apiKeys: secrets.NewMemoryApiKeyStore(), oauthForfait: secrets.NewMemoryOAuthStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger(),
		usageCaps:        usagecap.NewMemStore(),
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{}, nil),
	}
	preview, err := previewReadOnly(p).PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), envFundedWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("env-funded preview: candidates=%d pool={considered:%v reason:%q}", len(preview.Candidates), preview.Pool.Considered, preview.Pool.Reason)
	if len(preview.Candidates) == 0 && preview.Pool.Reason == "credentials_present" {
		t.Fatalf("the pool block claims %q with zero candidates present — the class 31cdebc12 fixed, on the env-funded path", preview.Pool.Reason)
	}
}
