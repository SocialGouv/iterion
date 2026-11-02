package model

// The serial half of the env seam's witnesses (#2808): seeding the
// PROCESS environment is global state — the parallel campaign's table
// (parallel_campaign_test.go) owns this file, and nothing here calls
// t.Parallel. What only a process seed can prove: a carried environment
// is hermetic AGAINST the process (the fallback mutation — reading
// os.Getenv for a name the map does not hold — turns the first witness
// red), and the process stays the default when nothing is carried.

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// A name seeded in the process does NOT read through a carried
// environment that does not hold it.
func TestEnvSeamIsHermeticAgainstTheProcess(t *testing.T) {
	name := fmt.Sprintf("ITERION_ENVSEAM_WITNESS_%d", os.Getpid())
	if err := os.Setenv(name, "process-value"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(name) })

	ctx := withCarriedEnv(context.Background(), carriedEnv{"ITERION_ENVSEAM_OTHER": "x"})
	if got := envGetenv(ctx, name); got != "" {
		t.Errorf("the process-seeded %s read %q through the carried environment: a carried test saw the process", name, got)
	}
	if _, ok := envLookupEnv(ctx, name); ok {
		t.Errorf("envLookupEnv(%s) = planted through the carried environment: the process leaked", name)
	}
	for _, pair := range envEnviron(ctx) {
		if pair == name+"=process-value" {
			t.Errorf("envEnviron carried %s: the process leaked into the whole-env read", pair)
		}
	}
}

// The process stays the default: nothing carried, the process answers.
func TestEnvSeamDefaultsToTheProcess(t *testing.T) {
	name := fmt.Sprintf("ITERION_ENVSEAM_DEFAULT_%d", os.Getpid())
	t.Setenv(name, "from-process")
	if got := envGetenv(context.Background(), name); got != "from-process" {
		t.Errorf("nothing carried: envGetenv = %q, want the process value", got)
	}
	if v, ok := envLookupEnv(context.Background(), name); !ok || v != "from-process" {
		t.Errorf("nothing carried: envLookupEnv = %q,%v, want the process value", v, ok)
	}
	// A nil carried environment is a no-op, not an empty one.
	if got := envGetenv(withCarriedEnv(context.Background(), nil), name); got != "from-process" {
		t.Errorf("nil carried env: envGetenv = %q, want the process value", got)
	}
}
