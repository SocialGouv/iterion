package pricingtest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func TestIsolateDoesNotReadAPlantedTempCache(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, root)
	}
	// Isolate is normally process-wide; restore its switch in this unit test.
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	path := filepath.Join(root, "iterion-modelspecs-test-absent.json")
	want := modelspecs.Spec{InputCostPerM: 0.125, OutputCostPerM: 0.25}
	data, err := json.Marshal(map[string]any{
		"fetched_at": time.Now(), "source": "pricingtest-fixture",
		"specs": map[string]modelspecs.Spec{"fixture/isolation": want},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	control := modelspecs.New(modelspecs.Options{CachePath: path, NoAutoFetch: true})
	if got, ok := control.LookupExact("fixture", "isolation"); !ok || got.InputCostPerM != want.InputCostPerM {
		t.Fatalf("cache control did not read its fixture: %+v, found=%v", got, ok)
	}
	t.Cleanup(Isolate())
	if got, ok := modelspecs.Default().LookupExact("fixture", "isolation"); ok {
		t.Fatalf("isolated registry read a planted temp cache: %+v", got)
	}
	// Tests that explicitly supply a registry retain that capability.
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{"fixture/isolation": want})))
	if got, ok := modelspecs.Default().LookupExact("fixture", "isolation"); !ok || got.InputCostPerM != want.InputCostPerM {
		t.Fatalf("explicit seeded registry was lost: %+v, found=%v", got, ok)
	}
}

// A forced refresh — `iterion models --refresh`, the server's ?refresh=1 —
// must not reach models.dev from an isolated binary.
func TestIsolateRefusesAForcedRefresh(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Cleanup(Isolate())
	if err := modelspecs.Default().Refresh(context.Background()); !errors.Is(err, modelspecs.ErrSeededRegistryNeverFetches) {
		t.Fatalf("Refresh on the isolated registry = %v, want ErrSeededRegistryNeverFetches: a forced refresh would fetch models.dev", err)
	}
}
