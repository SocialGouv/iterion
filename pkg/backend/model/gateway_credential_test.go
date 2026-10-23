package model

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestResolveWithContext_GatewayCredentialAndAmbientRefusal pins the
// precedence a run-held tenant gateway credential and the AmbientRefused
// stamp impose on `openai_compatible/…` resolution (#2005, ADR-131 §7):
//
//   - the run's gateway pair (key + base URL, default or pinned channel)
//     serves the route on a client built fresh per call — the registry
//     cache is keyed by the spec alone and one runner pod serially serves
//     many tenants, so the credential's client must never reach it;
//   - a gateway key met without its URL is the typed
//     secrets.ErrGatewayKeyWithoutURL, never a key sent to the
//     environment's gateway;
//   - under Credentials.AmbientRefused the pod environment is refused with
//     *ErrAmbientCredentialRefused: the run's own credentials fund its LLM
//     routes;
//   - with no credentials in the ctx and no stamp, resolution is unchanged:
//     the env factory serves the route as before.
//
// Mutations that must redden it:
//   - build the credential client from compatgw.FromEnv, or serve it
//     through r.cache → cases (a), (b), (d) answer with the decoy pod
//     gateway;
//   - move the gateway arm below the keyed-factory dispatch, where it is
//     unreachable for a provider with no keyed factory → cases (a)-(e)
//     fail;
//   - drop the ambient gates → case (e) returns the decoy env client.
func TestResolveWithContext_GatewayCredentialAndAmbientRefusal(t *testing.T) {
	// The registry's credentials path reads the INSTALLED lookup (what the
	// runner installs at boot): without it hasCreds is false even with
	// credentials on the context, and the test would exercise the wrong arm.
	installRunLookups(t)
	const (
		decoyURL  = "https://decoy-gw.example/pod"
		decoyKey  = "sk-pod-decoy"
		credURL   = "https://tenant-gw.example/v1"
		credKey   = "sk-tenant-gw"
		pinnedURL = "https://pinned-gw.example/v1"
		pinnedKey = "sk-pinned-gw"
	)
	cases := []struct {
		name string
		// stamp carries creds on the ctx; without it the ctx holds no
		// credentials at all. AmbientRefused inside creds is the stamp —
		// the flag travels on Credentials, so "no keys" is how a run is
		// stamped without holding one.
		stamp bool
		creds secrets.Credentials
		// decoy plants the pod environment's gateway the run must not reach.
		decoy bool
		// wantGateway: the client carries wantKey/wantURL, never the decoy.
		wantGateway      bool
		wantKey, wantURL string
		// wantWithoutURL: secrets.ErrGatewayKeyWithoutURL, no client.
		wantWithoutURL bool
		// wantAmbient: *ErrAmbientCredentialRefused, no client.
		wantAmbient bool
		// wantEnv: today's env resolution, the decoy values.
		wantEnv bool
	}{
		{
			// (a) the run's own gateway pair outranks the environment.
			name:  "the run's gateway credential serves the route",
			stamp: true,
			creds: secrets.Credentials{
				APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: credKey},
				APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: credURL},
			},
			wantGateway: true,
			wantKey:     credKey,
			wantURL:     credURL,
		},
		{
			// (b) the pinned channel, the default one empty: the same pair
			// rule, read through GatewayCredentialForRoute's one-map order.
			name:  "the pinned gateway credential serves the route with no default channel",
			stamp: true,
			creds: secrets.Credentials{
				PinnedAPIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: pinnedKey},
				PinnedAPIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: pinnedURL},
			},
			decoy:       true,
			wantGateway: true,
			wantKey:     pinnedKey,
			wantURL:     pinnedURL,
		},
		{
			// (c) a key sealed without its URL is refused, never paired with
			// the environment's gateway.
			name:  "a gateway key without its URL is the typed refusal",
			stamp: true,
			creds: secrets.Credentials{
				APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: credKey},
			},
			wantWithoutURL: true,
		},
		{
			// (d) the decoy pod env loses to the run's pair.
			name:  "the pod environment's gateway loses to the run's credential",
			stamp: true,
			creds: secrets.Credentials{
				APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: credKey},
				APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: credURL},
			},
			decoy:       true,
			wantGateway: true,
			wantKey:     credKey,
			wantURL:     credURL,
		},
		{
			// (e) the stamp refuses the environment: the run's credentials
			// fund its routes, and the pod's gateway is never a fallback.
			name:        "the ambient stamp refuses the pod environment",
			stamp:       true,
			creds:       secrets.Credentials{AmbientRefused: true},
			decoy:       true,
			wantAmbient: true,
		},
		{
			// (f) no stamp, no credentials: today's env resolution,
			// byte for byte.
			name:    "no credentials and no stamp resolve from the environment as before",
			decoy:   true,
			wantEnv: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installRunLookups(t)
			clearGatewayEnv(t)
			if tc.decoy {
				t.Setenv(compatgw.BaseURLEnv, decoyURL)
				t.Setenv(compatgw.APIKeyEnv, decoyKey)
			}
			ctx := context.Background()
			if tc.stamp {
				ctx = secrets.WithCredentials(ctx, tc.creds)
			}
			client, err := NewRegistry().ResolveWithContext(ctx, modelroute.OpenAICompatible+"/team-a/m")
			switch {
			case tc.wantGateway:
				if err != nil {
					t.Fatalf("ResolveWithContext: %v", err)
				}
				if got := clientStringField(t, client, "BaseURL"); got != tc.wantURL {
					t.Fatalf("BaseURL = %q, want the credential's own URL", got)
				}
				if got := clientStringField(t, client, "APIKey"); got != tc.wantKey {
					t.Fatalf("APIKey = %q, want the credential's own key", got)
				}
				if tc.decoy {
					if clientStringField(t, client, "BaseURL") == decoyURL {
						t.Fatal("the resolved client dials the decoy pod gateway")
					}
					if clientStringField(t, client, "APIKey") == decoyKey {
						t.Fatal("the resolved client authenticates with the decoy pod key")
					}
				}
			case tc.wantWithoutURL:
				if client != nil {
					t.Fatalf("client = %v, want nil beside the refusal", client)
				}
				if !errors.Is(err, secrets.ErrGatewayKeyWithoutURL) {
					t.Fatalf("err = %v, want secrets.ErrGatewayKeyWithoutURL", err)
				}
			case tc.wantAmbient:
				if client != nil {
					t.Fatalf("client = %v, want nil beside the refusal", client)
				}
				var refused *ErrAmbientCredentialRefused
				if !errors.As(err, &refused) {
					t.Fatalf("err = %v, want *ErrAmbientCredentialRefused", err)
				}
				if refused.Provider != modelroute.OpenAICompatible {
					t.Errorf("refusal names provider %q, want the route's provider", refused.Provider)
				}
			case tc.wantEnv:
				if err != nil {
					t.Fatalf("ResolveWithContext: %v", err)
				}
				if got := clientStringField(t, client, "BaseURL"); got != decoyURL {
					t.Errorf("BaseURL = %q, want the env gateway unchanged", got)
				}
				if got := clientStringField(t, client, "APIKey"); got != decoyKey {
					t.Errorf("APIKey = %q, want the env key unchanged", got)
				}
			}
		})
	}

	// (g) a vendor route with no credentials in the ctx is unchanged: no
	// gateway arm applies, no stamp is set, and the env factory answers
	// exactly as before.
	t.Run("a vendor route with no credentials is unchanged", func(t *testing.T) {
		installRunLookups(t)
		clearGatewayEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env")
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
		t.Setenv("ZAI_API_KEY", "")
		client, err := NewRegistry().ResolveWithContext(context.Background(), "anthropic/claude-sonnet-4-6")
		if err != nil {
			t.Fatalf("ResolveWithContext: %v", err)
		}
		if got := clientStringField(t, client, "BaseURL"); got != "https://api.anthropic.com" {
			t.Errorf("BaseURL = %q, want the vendor endpoint unchanged", got)
		}
		if got := clientStringField(t, client, "APIKey"); got != "sk-ant-env" {
			t.Errorf("APIKey = %q, want the env key unchanged", got)
		}
	})
}

// Under the ambient stamp a run's z.ai key authenticates to z.ai's own
// endpoint, never the pod's ANTHROPIC_BASE_URL — the same choice the facade
// path makes (facadeEnvForRunHeld).
func TestResolveWithContext_GLMUnderStampTakesTheVendorEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://pod-gateway.example/meter")
	// The dispatch wires the widened hint (RouteProviderHint: zai for a GLM
	// id on the anthropic wire) beside the run's credentials, and a pod has
	// the lookups installed — this is the production shape.
	installRunLookups(t)
	ctx := WithProviderHint(secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderZAI: "sk-run-zai"},
	}), "zai")
	cl, err := NewRegistry().ResolveWithContext(ctx, "anthropic/glm-5.2")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	v := reflect.ValueOf(cl)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if got := v.FieldByName("BaseURL").String(); got != secrets.ZAIDefaultBaseURL {
		t.Fatalf("BaseURL = %q, want z.ai's own endpoint — the pod's ANTHROPIC_BASE_URL is outside the run's trust domain", got)
	}
	if got := v.FieldByName("APIKey").String(); got != "sk-run-zai" {
		t.Fatalf("APIKey = %q, want the run's z.ai key", got)
	}
}

// The crossing twin: a stamped run's glm node takes z.ai's own endpoint in
// the container too — the pod's ANTHROPIC_BASE_URL crosses as explicit-empty.
func TestForwardableProviderEnv_GLMUnderStampCrossesTheVendorEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://pod-gateway.example/meter")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderZAI: "sk-run-zai"},
	})
	env, err := forwardableProviderEnv(ctx, "anthropic/glm-5.2", "")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if v := env["ANTHROPIC_BASE_URL"]; v != "" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want the explicit empty — the pod's routing env is outside the run's trust domain", v)
	}
}

// A run-held key without its URL is the typed pairing refusal at the seam —
// the registry answers the same state with the same error, never the pod's
// env gateway.
func TestCheckGatewaySource_KeyWithoutURLIsTheTypedRefusal(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://93.184.216.34/v1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "env-gw")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "sk-unpaired"},
	})
	err := checkGatewaySource(ctx, "openai_compatible/m", false)
	if !errors.Is(err, secrets.ErrGatewayKeyWithoutURL) {
		t.Fatalf("err = %v, want the typed pairing refusal — the pod's env gateway must not serve a run that held a broken pair", err)
	}
}

// The run-side LLM client resolvers answer to the run's credentials: under
// the stamp a vendor spec is the typed refusal, which only
// ResolveWithContext answers. A site that drops back to bare Resolve serves
// the pod's env key to a stamped run.
func TestRunSideClientResolversUseTheContextVariant(t *testing.T) {
	for _, f := range []string{"executor_human.go", "executor_build_task.go", "subagent.go"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !strings.Contains(string(body), "ResolveWithContext(ctx, modelSpec)") {
			t.Fatalf("%s resolves its client without the run's credentials (bare Resolve)", f)
		}
		if strings.Contains(string(body), "registry.Resolve(modelSpec)") || strings.Contains(string(body), "modelReg.Resolve(modelSpec)") {
			t.Fatalf("%s still calls bare Resolve beside the context variant", f)
		}
	}
}
