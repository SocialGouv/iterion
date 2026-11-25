package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The description tells the model WHEN a program pays (#3653). Two
// measurements on the production model found the tool misused without it:
// one small program per turn and never call.parallel (#3643), or never
// called at all (#3650). These are the clauses the re-measurement judges.
func TestTheDescriptionSaysWhenAProgramPays(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"every direct call costs a turn",
		"ONE program for a dependent chain",
		"batch independent calls with call.parallel in ONE program",
		"never the raw output",
		"saves nothing: call that tool directly",
	} {
		if !strings.Contains(codemodeDescription, want) {
			t.Errorf("the tool description does not say %q", want)
		}
	}
	if !strings.HasSuffix(codemodeDescription, codemodeExample) {
		t.Error("the description does not end with the example the host is proven to run")
	}
}

// The example is executed, not trusted: a form the host refuses (an await,
// a fifth parallel entry, a promise) would teach the model to lose the whole
// program. It runs through the real child host and the node's door, against
// a search tool and a read tool shaped like the ones it names.
func TestTheDescriptionsExampleRunsOnTheRealHost(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var read []string
	toolMap := map[string]*GenerationTool{
		"grep": {Name: "grep", Execute: func(context.Context, json.RawMessage) (string, error) {
			return "a.go:3:TODO first\nb.go:9:TODO second\n", nil
		}},
		"read_file": {Name: "read_file", Execute: func(_ context.Context, in json.RawMessage) (string, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return "", err
			}
			mu.Lock()
			read = append(read, args.Path)
			mu.Unlock()
			return "contents of " + args.Path, nil
		}},
	}
	out, err := runBounded(t, scripthost.Limits{}, toolMap, codemodeExample)
	if err != nil {
		t.Fatalf("the description's example did not run: %v", err)
	}
	var got []struct {
		Hit  string `json:"hit"`
		Head string `json:"head"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the example returned %q, not the JSON array it promises: %v", out, err)
	}
	if len(got) != 2 {
		t.Fatalf("the example returned %d entries, want one per search hit (2): %q", len(got), out)
	}
	// Keyed by the hit each entry carries, never by completion order.
	want := map[string]string{"a.go:3:TODO first": "contents of a.go", "b.go:9:TODO second": "contents of b.go"}
	for _, e := range got {
		if want[e.Hit] != e.Head {
			t.Errorf("entry %q carries %q, want %q", e.Hit, e.Head, want[e.Hit])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(read) != 2 {
		t.Errorf("the read tool ran %d times (%v), want once per hit", len(read), read)
	}
}
