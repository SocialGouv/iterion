package modelspecs

import (
	"strings"
	"testing"
	"time"
)

// models.dev states a model's input modalities; the image flag is read from
// them: a list naming "image" is true, a list without it false, and a row
// that states none stays unknown — never a guessed false, which would make a
// working vision route refuse images.
func TestParseModelsDev_ImageInputFromModalities(t *testing.T) {
	body := []byte(`{"scaleway":{"models":{
		"glm-5.3":{"modalities":{"input":["text"],"output":["text"]}},
		"mistral-small-3.2-24b-instruct-2506":{"modalities":{"input":["text","image"],"output":["text"]}},
		"no-modalities":{"limit":{"context":1000}},
		"empty-input":{"modalities":{"input":[]}},
		"upper":{"modalities":{"input":["TEXT","Image"]}}
	}}}`)
	flat, err := ParseModelsDev(body)
	if err != nil {
		t.Fatalf("ParseModelsDev: %v", err)
	}
	for key, want := range map[string]string{
		"scaleway/glm-5.3": "false",
		"scaleway/mistral-small-3.2-24b-instruct-2506": "true",
		"scaleway/no-modalities":                       "unstated",
		"scaleway/empty-input":                         "unstated",
		"scaleway/upper":                               "true",
	} {
		if got := flagWord(flat[key].ImageInput); got != want {
			t.Errorf("%s: image_input %s, want %s", key, got, want)
		}
	}
}

// The bare index folds publishers by agreement: glm-5.3 is text-only at one
// provider and takes images at another, so the bare answer is unknown, not
// either side. A single publisher, or unanimous ones, keep their answer.
func TestConsensus_ImageInputDisagreementIsUnknown(t *testing.T) {
	r := &Registry{}
	r.indexLocked(map[string]Spec{
		"scaleway/glm-5.3":  {ImageInput: boolPtr(false)},
		"other/glm-5.3":     {ImageInput: boolPtr(true)},
		"a/vision-x":        {ImageInput: boolPtr(true)},
		"b/vision-x":        {ImageInput: boolPtr(true)},
		"a/text-y":          {ImageInput: boolPtr(false)},
		"b/text-y":          {},
		"solo/only-one":     {ImageInput: boolPtr(false)},
		"scaleway/qwen3.6x": {ImageInput: boolPtr(true)},
	})
	for bare, want := range map[string]string{
		"glm-5.3":  "unstated",
		"vision-x": "true",
		"text-y":   "unstated",
		"only-one": "false",
		"qwen3.6x": "true",
	} {
		if got := flagWord(r.byModel[bare].ImageInput); got != want {
			t.Errorf("bare %s: image_input %s, want %s", bare, got, want)
		}
	}
	// The provider-qualified key keeps its own publisher's answer.
	if got := flagWord(r.byFull["scaleway/glm-5.3"].ImageInput); got != "false" {
		t.Errorf("scaleway/glm-5.3 exact: image_input %s, want false", got)
	}
}

// The snapshot carries image_input through its one writer and its one reader,
// a stated false included, and leaves it absent where models.dev states no
// modality.
func TestSnapshot_ImageInputRoundTrips(t *testing.T) {
	raw, err := WriteSnapshot([]byte(`{"scaleway":{"models":{
		"glm-5.3":{"modalities":{"input":["text"]}},
		"gemma-4-26b-a4b-it":{"modalities":{"input":["text","image"]}},
		"bare":{}
	}}}`), "sha256:test", time.Now().UTC())
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if strings.Count(string(raw), `"image_input"`) != 2 {
		t.Fatalf("the snapshot states image_input on %d lines, want 2 (absent where unstated):\n%s", strings.Count(string(raw), `"image_input"`), raw)
	}
	_, table, err := loadFrom(raw)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	for key, want := range map[string]string{
		"scaleway/glm-5.3":            "false",
		"scaleway/gemma-4-26b-a4b-it": "true",
		"scaleway/bare":               "unstated",
	} {
		if got := flagWord(table[key].ImageInput); got != want {
			t.Errorf("%s: image_input %s after the round trip, want %s", key, got, want)
		}
	}
}
