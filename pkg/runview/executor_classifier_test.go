package runview

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Under the AmbientRefused stamp the classifier's environment fallback is
// refused by type: the run's sealed credentials fund its auxiliaries, and a
// vendor classifier spec on a stamped run names the refusal at build time —
// a classifier that would classify every call against the pod environment
// must not be built. A GATEWAY classifier spec resolves from the run's
// sealed pair instead (uncached).
func TestNewLLMClassifierFromEnv_AmbientRefusedAndGatewaySpec(t *testing.T) {
	// The registry's credentials path reads the INSTALLED lookup (the
	// runner installs it at boot): without it hasCreds is false even with
	// credentials on the context, and the gateway arm never runs.
	model.SetCredentialsLookup(model.RunCredentialsLookup)
	model.SetOAuthDirLookup(model.RunOAuthDirLookup)
	t.Cleanup(func() {
		model.SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		model.SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})
	t.Setenv("ITERION_LLM_CLASSIFIER_MODEL", "anthropic/claude-haiku-4-5")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	reg := model.NewRegistry()
	_, err := newLLMClassifierFromEnv(ctx, reg, nil)
	var ambient *model.ErrAmbientCredentialRefused
	if !errors.As(err, &ambient) {
		t.Fatalf("err = %v, want *model.ErrAmbientCredentialRefused (a vendor classifier on a stamped run is refused at build)", err)
	}

	// The gateway twin: the classifier spec names the gateway, the run's
	// sealed pair serves it, and the resolved client points at the run's
	// gateway — never at an environment variable.
	t.Setenv("ITERION_LLM_CLASSIFIER_MODEL", "openai_compatible/haiku")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://decoy-gw.example/pod")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-pod-decoy")
	ctx = secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "sk-run-classifier"},
		APIBaseURLs:    map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://run-gw.example"},
	})
	classifier, err := newLLMClassifierFromEnv(ctx, reg, nil)
	if err != nil {
		t.Fatalf("a gateway classifier spec refused under the stamp: %v", err)
	}
	_ = classifier
}

// Four production surfaces construct an ExecutorSpec with a nil Ctx (the
// studio launch and resume, both CLI paths): the classifier must not panic
// on it — the ambient refusal is a decision, a nil dereference is a crash.
func TestNewLLMClassifierFromEnv_ANilCtxDoesNotPanic(t *testing.T) {
	t.Setenv("ITERION_LLM_CLASSIFIER_MODEL", "anthropic/claude-haiku-4-5")
	classifier, err := newLLMClassifierFromEnv(nil, model.NewRegistry(), iterlog.Nop())
	if err == nil && classifier == nil {
		t.Fatal("a nil ctx built neither a classifier nor a refusal")
	}
}
