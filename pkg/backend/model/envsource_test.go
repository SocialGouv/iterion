package model

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The seam's reason to exist (#2808): tests that run in parallel cannot
// t.Setenv (the testing package panics), so each carries its own
// environment on the context and reads its own values back. Mutations
// this witnesses against: the seam ignoring the carried environment (both
// subtests read the process instead — red) and a SHARED environment (each
// subtest reads the other's value — red). The hermeticity against the
// PROCESS environment is the serial sibling file's witness
// (envsource_process_test.go — seeding the process is global state, the
// parallel campaign's table owns it).
func TestEnvSeamParallelTestsReadTheirOwnValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value, other string }{
		{"a", "value-a", "b"},
		{"b", "value-b", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := withCarriedEnv(context.Background(), carriedEnv{
				"ITERION_ENVSEAM_SHARED_NAME":                      tc.value,
				"ITERION_ENVSEAM_ONLY_" + strings.ToUpper(tc.name): "mine-" + tc.name,
			})
			if got := envGetenv(ctx, "ITERION_ENVSEAM_SHARED_NAME"); got != tc.value {
				t.Errorf("the same name, two parallel values: read %q, want my own %q", got, tc.value)
			}
			if got := envGetenv(ctx, "ITERION_ENVSEAM_ONLY_"+strings.ToUpper(tc.other)); got != "" {
				t.Errorf("a name only the SIBLING carries read %q through my environment: the seam leaked across tests", got)
			}
			if _, ok := envLookupEnv(ctx, "ITERION_ENVSEAM_NEVER_SET"); ok {
				t.Error("LookupEnv of a name neither side holds: planted=true")
			}
			if v, ok := envLookupEnv(ctx, "ITERION_ENVSEAM_SHARED_NAME"); !ok || v != tc.value {
				t.Errorf("envLookupEnv = %q,%v, want %q,true (planted reads through)", v, ok, tc.value)
			}
		})
	}
}

// The whole-env shape: sorted k=v pairs, and ONLY what the map holds.
func TestCarriedEnvEnviron(t *testing.T) {
	t.Parallel()
	ctx := withCarriedEnv(context.Background(), carriedEnv{"B": "2", "A": "1"})
	got := envEnviron(ctx)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Fatalf("envEnviron = %v, want [A=1 B=2] (sorted, hermetic)", got)
	}
	if got := envEnviron(context.Background()); len(got) == 0 {
		t.Fatal("nothing carried: envEnviron must answer the process environment")
	}
	// An empty non-nil map is a deliberately empty environment — not the
	// process default.
	empty := withCarriedEnv(context.Background(), carriedEnv{})
	if got := envEnviron(empty); len(got) != 0 {
		t.Fatalf("empty carried env: envEnviron = %v, want empty", got)
	}
	// A name nothing seeded anywhere (unique per run) reads unset through
	// the empty environment — the process is not consulted.
	never := fmt.Sprintf("ITERION_ENVSEAM_EMPTY_%d", os.Getpid())
	if got := envGetenv(empty, never); got != "" {
		t.Errorf("empty carried env: envGetenv(%s) = %q, want unset", never, got)
	}
}

// The security half of the seam (#2808): secretguard's ambient-secret
// enumeration (the whole-env reader) and its kill-switch read through
// it. A secret whose env name is seen through the carried environment is
// STILL redacted; the mutant — the seam's Environ answering empty —
// turns the redaction assertion red, not a compile error.
func TestBuildSecretGuardRedactsThroughTheCarriedEnv(t *testing.T) {
	t.Parallel()
	const value = "sk-acme-9f8e7d6c5b4a39281706f5e4d3c2b1a0987654321fedcba"
	ctx := withCarriedEnv(context.Background(), carriedEnv{
		"ACME_ENVSEAM_API_KEY": value,
	})
	guard := BuildSecretGuard(ctx, &ir.Workflow{}, nil, nil)
	if guard == nil {
		t.Fatal("redaction enabled through the seam: BuildSecretGuard returned nil (ITERION_SECRETS_REDACT=off would do that — is the seam leaking one?)")
	}
	redacted := guard.Redact("the key is " + value + " ok")
	if strings.Contains(redacted, value) {
		t.Fatalf("the ambient secret read through the seam was NOT redacted: %q — the seam answering empty here is a secret in a log", redacted)
	}

	// The kill-switch through the seam too: ITERION_SECRETS_REDACT=off
	// disables SINK redaction (the guard stays live for materialisation —
	// Layer 1's mirror never follows the sink switch), so Redact leaves
	// the carried value untouched. Proves the named reads take the seam
	// as well as the whole-env one.
	ctxOff := withCarriedEnv(context.Background(), carriedEnv{
		"ACME_ENVSEAM_API_KEY":   value,
		"ITERION_SECRETS_REDACT": "off",
	})
	if guardOff := BuildSecretGuard(ctxOff, &ir.Workflow{}, nil, nil); guardOff != nil {
		if out := guardOff.Redact("the key is " + value + " ok"); !strings.Contains(out, value) {
			t.Fatalf("ITERION_SECRETS_REDACT=off through the seam must leave the value in the sink, got %q — the named reads bypass the seam", out)
		}
	}
}
