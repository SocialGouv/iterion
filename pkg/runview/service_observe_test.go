package runview

import (
	"bytes"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// The ambient twin: on a run whose credentials carry the AmbientRefused
// stamp, only gateway-model supervisors survive the spawn walk — a
// vendor-family supervisor resolves through the pod environment, which the
// stamp refuses, and would spin refusal-per-evaluation for a run that must
// not spend it. An unstamped run keeps every spec.
func TestAmbientSurvivingSpecs_KeepsGatewayDropsVendorWhenRefused(t *testing.T) {
	specs := []supervise.Spec{
		{Name: "gw", Model: "openai_compatible/glm-5.3"},
		{Name: "vendor", Model: "anthropic/claude-opus-4-8"},
		{Name: "unpinned", Model: ""},
	}
	logs := &bytes.Buffer{}
	logger := iterlog.New(iterlog.LevelInfo, logs)

	kept := AmbientSurvivingSpecs(specs, false, logger)
	if len(kept) != 3 {
		t.Fatalf("an unstamped run keeps every spec: %+v", kept)
	}

	kept = AmbientSurvivingSpecs(specs, true, logger)
	if len(kept) != 1 || kept[0].Name != "gw" {
		t.Fatalf("a refused run kept %+v, want only the gateway-model supervisor", kept)
	}
	for _, want := range []string{"vendor", "unpinned"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the dropped supervisor %q is not named in the log", want)
		}
	}
	if !strings.Contains(logs.String(), "pod environment") {
		t.Error("the drop reason does not name the refused environment")
	}
}
