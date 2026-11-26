package compatgw

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func imageWord(b *bool) string {
	switch {
	case b == nil:
		return "unstated"
	case *b:
		return "true"
	}
	return "false"
}

// The PLIAGE case: a gateway naming its routes `scaleway/<id>`, its catalog
// provider scaleway. The provider-qualified models.dev row answers the image
// question from the embedded snapshot: glm-5.3 is text-only, the vision
// models PLIAGE serves take images. An id no table carries stays unstated.
func TestResolveCatalog_ImageInputFromTheProviderQualifiedRow(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogProviderEnv: "scaleway"})
	for id, want := range map[string]string{
		"scaleway/glm-5.3": "false",
		"scaleway/mistral-small-3.2-24b-instruct-2506": "true",
		"scaleway/mistral-medium-3.5-128b":             "true",
		"scaleway/gemma-4-26b-a4b-it":                  "true",
		"scaleway/qwen3.5-397b-a17b":                   "true",
		"scaleway/qwen3.6-35b-a3b":                     "true",
		"scaleway/deepseek-v4.1-flash-1017":            "true",
		"scaleway/no-such-model":                       "unstated",
	} {
		if got := imageWord(ResolveCatalogQuiet(id, nil, get).Spec.ImageInput); got != want {
			t.Errorf("%s: image_input %s, want %s", id, got, want)
		}
	}
}

// The host's answer crosses into the sandbox in the record, a stated false
// included, and the container reads it back as the host resolved it.
func TestForwardableRecord_ImageInputCrossesTheEnv(t *testing.T) {
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	for _, stated := range []*bool{new(bool), func() *bool { b := true; return &b }(), nil} {
		ResetCatalogCaches()
		host := Resolved{Spec: modelspecs.Spec{ContextWindow: 1000, ImageInput: stated}, Source: SourceOperator}
		raw, err := json.Marshal(ForwardableRecord("gw/m", host))
		if err != nil {
			t.Fatal(err)
		}
		got := ResolveCatalog("gw/m", nil, mapGet(map[string]string{ResolvedEnvName: string(raw)}))
		if got.Source != SourceOperator || imageWord(got.Spec.ImageInput) != imageWord(stated) {
			t.Fatalf("the container read %s from %s, want the host's %s", imageWord(got.Spec.ImageInput), raw, imageWord(stated))
		}
	}
}

// A record a workspace .env planted carries no image verdict either: the
// repository under review cannot make the reviewing node refuse images, nor
// accept them.
func TestResolveCatalog_APlantedRecordStatesNoImageInput(t *testing.T) {
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, ResolvedEnvName)
	envtrust.ResetForTest()
	ResetCatalogCaches()
	get := mapGet(map[string]string{ResolvedEnvName: `{"model":"gw/m","source":"operator","image_input":false}`})
	if res := ResolveCatalog("gw/m", nil, get); res.Spec.ImageInput != nil {
		t.Fatalf("a planted record stated image_input %s", imageWord(res.Spec.ImageInput))
	}
}

// The operator table refuses image_input as it refuses reasoning_efforts: the
// launch seam names the field, and the table answers nothing.
func TestOperatorModels_RefusesImageInput(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	get := mapGet(map[string]string{CatalogModelsEnv: `{"gw/text":{"image_input":false}}`})
	if err := CheckOperatorTable(get); err == nil || !strings.Contains(err.Error(), `unknown field "image_input"`) {
		t.Fatalf("the operator table accepted image_input: err = %v", err)
	}
	if res := ResolveCatalogQuiet("gw/text", nil, get); res.Known() {
		t.Fatalf("a refused table answered: %+v", res)
	}
}

// The rung that answers an id answers its image flag too, whole: an operator
// row for scaleway/glm-5.3 answers and its image flag is unknown — the
// models.dev row's false is hidden, images are sent — and a priced feed entry
// does the same unless it states image_input, which is how such an id is
// made to refuse images (the documented remedy in image-acceptance.md).
func TestResolveCatalog_TheAnsweringRungAnswersTheImageFlag(t *testing.T) {
	ResetCatalogCaches()
	silent := mapGet(map[string]string{
		CatalogProviderEnv: "scaleway",
		CatalogModelsEnv:   `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`,
	})
	if res := ResolveCatalogQuiet("scaleway/glm-5.3", nil, silent); res.Source != SourceOperator || res.Spec.ImageInput != nil {
		t.Fatalf("an operator row resolved source %s image_input %s, want operator and unstated", res.Source, imageWord(res.Spec.ImageInput))
	}
	// The run's feed answers before both, whole: a priced feed entry that
	// states no image_input hides the scaleway row too; stating it refuses,
	// over an operator row for the same id as well.
	for _, tc := range []struct {
		spec, want string
		operator   bool
	}{
		{`{"input_cost_per_m":2.01,"output_cost_per_m":6.15}`, "unstated", false},
		{`{"input_cost_per_m":2.01,"output_cost_per_m":6.15,"image_input":false}`, "false", false},
		{`{"input_cost_per_m":2.01,"output_cost_per_m":6.15,"image_input":false}`, "false", true},
	} {
		ResetCatalogCaches()
		table := `{"entries":{"scaleway/glm-5.3":{"feed":"pliage","refreshed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","fresh_for":7200,"spec":` + tc.spec + `}}}`
		env := map[string]string{CatalogProviderEnv: "scaleway", FeedTableEnv: table}
		if tc.operator {
			env[CatalogModelsEnv] = `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`
		}
		res := ResolveCatalogQuiet("scaleway/glm-5.3", nil, mapGet(env))
		if res.Source != SourceFeed("pliage") || imageWord(res.Spec.ImageInput) != tc.want {
			t.Fatalf("feed entry %s (operator row %v) resolved source %s image_input %s, want feed:pliage and %s", tc.spec, tc.operator, res.Source, imageWord(res.Spec.ImageInput), tc.want)
		}
	}
}
