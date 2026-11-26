package pricefeed

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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

// A feed's entries (declared, exec) accept image_input, so an operator can
// declare it for a gateway that publishes nothing; unstated stays nil. The
// operator table refuses it (TestTheOperatorTableRefusesImageInput).
func TestFeedEntriesAcceptImageInput(t *testing.T) {
	raw := `{"scaleway/glm-5.3":{"image_input":false},"scaleway/gemma":{"image_input":true},"other":{"context_window":8}}`
	table, err := ParseFeedEntries(raw, "test")
	if err != nil {
		t.Fatalf("ParseFeedEntries: %v", err)
	}
	for id, want := range map[string]string{"scaleway/glm-5.3": "false", "scaleway/gemma": "true", "other": "unstated"} {
		if got := imageWord(table[id].ImageInput); got != want {
			t.Errorf("%s: image_input %s, want %s", id, got, want)
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

// A declared entry's image_input reaches the run only when that entry is
// the one that answers: within a scope the FIRST enabled feed pricing the id
// answers it whole, so a declaration listed after a priced fetched feed
// changes nothing (unstated), listed first (`declare --first`) it answers
// false — with only the fields it restates — and at a more specific scope
// it answers over the less specific feed. An unpriced declaration steps
// aside for a priced entry.
func TestFoldImageInputAnswersFromTheAnsweringEntryOnly(t *testing.T) {
	const id = "scaleway/glm-5.3"
	now := time.Now()
	gw := fetchedFeed("gw")
	gwState := map[string]*StateRecord{"gw": freshState("gw", now, map[string]modelspecs.Spec{
		id: {InputCostPerM: 2.01, OutputCostPerM: 6.15, ContextWindow: 256000},
	})}
	priced := declaredFeed("pliage", `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15,"image_input":false}}`)
	unpriced := declaredFeed("pliage", `{"scaleway/glm-5.3":{"image_input":false}}`)
	scope := func(feeds ...FeedConfig) ScopeState {
		return ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: feeds}, States: gwState}
	}
	for _, tc := range []struct {
		name   string
		scopes []ScopeState
		want   string
		window int
	}{
		{"declared after a priced feed", []ScopeState{scope(gw, priced)}, "unstated", 256000},
		{"declared first", []ScopeState{scope(priced, gw)}, "false", 0},
		{"declared at a more specific scope", []ScopeState{scope(gw), {Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{priced}}}}, "false", 0},
		{"unpriced declaration first", []ScopeState{scope(unpriced, gw)}, "unstated", 256000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := Fold(FoldConfig{Now: now}, tc.scopes...)
			e, ok := table.Entries[id]
			if !ok || imageWord(e.Spec.ImageInput) != tc.want || e.Spec.ContextWindow != tc.window {
				t.Fatalf("entry %+v (present %v): image_input %s window %d, want %s and %d", e, ok, imageWord(e.Spec.ImageInput), e.Spec.ContextWindow, tc.want, tc.window)
			}
		})
	}
}
