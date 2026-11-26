package pricefeed

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

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

// A LiteLLM listing's supports_vision is the image_input flag: stated true,
// stated false, and absent (PLIAGE's /model/info states it for no model)
// left unstated, never read as false.
func TestLiteLLMSourceReadsSupportsVision(t *testing.T) {
	body := `{"data":[` +
		`{"model_name":"vision","model_info":{"supports_vision":true}},` +
		`{"model_name":"text","model_info":{"supports_vision":false}},` +
		`{"model_name":"silent","model_info":{"max_input_tokens":1000}},` +
		`{"model_name":"null","model_info":{"supports_vision":null}}]}`
	stubHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
	src, _ := SourceFor(TypeLiteLLM)
	table, err := src.Fetch(context.Background(), map[string]string{"url": os.Getenv("PRICEFEED_TEST_URL")})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"vision": "true", "text": "false", "silent": "unstated", "null": "unstated"} {
		if got := imageWord(table[id].ImageInput); got != want {
			t.Errorf("%s: image_input %s, want %s", id, got, want)
		}
	}
}

// Deployments of one model_name fold their supports_vision by the minimum of
// false < unstated < true, in both listing orders: one refusing deployment
// makes the group refuse, a true beside an unstated one stays unstated (the
// silent deployment may refuse), and only unanimous true is true.
func TestLiteLLMSourceFoldsSupportsVisionAcrossDeployments(t *testing.T) {
	yes := `{"model_name":"m","model_info":{"supports_vision":true}}`
	no := `{"model_name":"m","model_info":{"supports_vision":false}}`
	silent := `{"model_name":"m","model_info":{"max_input_tokens":1000}}`
	for _, tc := range []struct {
		name  string
		items []string
		want  string
	}{
		{"true and false", []string{yes, no}, "false"},
		{"false and true", []string{no, yes}, "false"},
		{"true and unstated", []string{yes, silent}, "unstated"},
		{"unstated and true", []string{silent, yes}, "unstated"},
		{"false and unstated", []string{no, silent}, "false"},
		{"unstated and false", []string{silent, no}, "false"},
		{"true and true", []string{yes, yes}, "true"},
		{"true, unstated, true", []string{yes, silent, yes}, "unstated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"data":[` + strings.Join(tc.items, ",") + `]}`
			stubHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
			src, _ := SourceFor(TypeLiteLLM)
			table, err := src.Fetch(context.Background(), map[string]string{"url": os.Getenv("PRICEFEED_TEST_URL")})
			if err != nil {
				t.Fatal(err)
			}
			if got := imageWord(table["m"].ImageInput); got != tc.want {
				t.Fatalf("folded image_input %s, want %s", got, tc.want)
			}
		})
	}
}

// The operator table and a feed's entries accept image_input, so an operator
// can declare it for a gateway that publishes nothing; unstated stays nil.
func TestEntriesAcceptImageInput(t *testing.T) {
	raw := `{"scaleway/glm-5.3":{"image_input":false},"scaleway/gemma":{"image_input":true},"other":{"context_window":8}}`
	for name, parse := range map[string]func(string, string) (map[string]modelspecs.Spec, error){
		"ParseEntries":     ParseEntries,
		"ParseFeedEntries": ParseFeedEntries,
	} {
		table, err := parse(raw, "test")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for id, want := range map[string]string{"scaleway/glm-5.3": "false", "scaleway/gemma": "true", "other": "unstated"} {
			if got := imageWord(table[id].ImageInput); got != want {
				t.Errorf("%s %s: image_input %s, want %s", name, id, got, want)
			}
		}
	}
	// A declared row stating only image_input states something: it answers.
	if got := unpricedFields(modelspecs.Spec{ImageInput: new(bool)}); len(got) != 1 || got[0] != "image_input" {
		t.Errorf("unpricedFields = %v, want [image_input]", got)
	}
	// A refresh flipping image_input is a change.
	if SpecEqual(modelspecs.Spec{ImageInput: new(bool)}, modelspecs.Spec{}) {
		t.Error("SpecEqual reads a stated image_input and an unstated one as equal")
	}
}
