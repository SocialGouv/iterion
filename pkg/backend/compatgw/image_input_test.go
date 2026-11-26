package compatgw

import (
	"encoding/json"
	"testing"

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

// The operator table declares image_input for a gateway that publishes
// nothing.
func TestOperatorModels_DeclaresImageInput(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogModelsEnv: `{"gw/text":{"image_input":false},"gw/any":{}}`})
	if got := imageWord(ResolveCatalog("gw/text", nil, get).Spec.ImageInput); got != "false" {
		t.Errorf("gw/text: image_input %s, want false", got)
	}
	if got := imageWord(ResolveCatalog("gw/any", nil, get).Spec.ImageInput); got != "unstated" {
		t.Errorf("gw/any: image_input %s, want unstated", got)
	}
}
