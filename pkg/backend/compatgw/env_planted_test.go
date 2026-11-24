package compatgw

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The private-address refusal names a planted hatch value: an operator
// whose self-hosted gateway stays refused must learn their .env does not
// speak for them, not re-export it into the same file.
func TestValidate_TheHatchRefusalNamesAPlantedValue(t *testing.T) {
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	envtrust.MarkPlanted("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE")

	cfg := Config{BaseURL: "http://127.0.0.1:9/v1"}
	err := cfg.Validate(true)
	if err == nil || !strings.Contains(err.Error(), "project .env") {
		t.Fatalf("err = %v, want the refusal naming the project .env as non-lifting", err)
	}

	// The honest operator path keeps the plain remedy line. The marker
	// goes with the planted set: ResetForTest re-derives it from the env.
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
	err = cfg.Validate(true)
	if err == nil || !strings.Contains(err.Error(), "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1") || strings.Contains(err.Error(), "project .env") {
		t.Fatalf("err = %v, want the plain escape-hatch remedy for an unplanted value", err)
	}
}
