package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// The deferral split (CM-14) reads the SAME state the visibility filter
// used: the catalogue-only set the dispatch pinned beside the executable
// one. A node that defers says so in the note, and a catalogue-only match is
// marked program_only without its schema — the declaration is what
// call.describe returns inside the program, not bytes the conversation
// re-spends. The same query against the same catalogue without the pin
// keeps today's direct-call wording, schema included.
func TestClawToolSearch_NotePerDeferState(t *testing.T) {
	newRegistry := func(t *testing.T) *Registry {
		t.Helper()
		r := NewRegistry()
		if err := RegisterClawBuiltinsWithScope(r, NewReadScope(t.TempDir()), nil); err != nil {
			t.Fatalf("register builtins: %v", err)
		}
		if err := RegisterClawToolSearch(r, func() []api.Tool { return registrySnapshot(r) }); err != nil {
			t.Fatalf("RegisterClawToolSearch: %v", err)
		}
		return r
	}
	type matchDetail struct {
		Name        string          `json:"name"`
		Available   bool            `json:"available"`
		ProgramOnly bool            `json:"program_only"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	search := func(t *testing.T, r *Registry, ctx context.Context, query string) (map[string]matchDetail, string) {
		t.Helper()
		td, err := r.Resolve("tool_search")
		if err != nil {
			t.Fatalf("resolve tool_search: %v", err)
		}
		in, _ := json.Marshal(map[string]any{"query": query})
		out, err := td.Execute(ctx, in)
		if err != nil {
			t.Fatalf("tool_search %q: %v", query, err)
		}
		var res struct {
			Tools []matchDetail `json:"tools"`
			Note  string        `json:"note"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("the per-node answer does not decode (%v): %s", err, out)
		}
		byName := make(map[string]matchDetail, len(res.Tools))
		for _, m := range res.Tools {
			byName[m.Name] = m
		}
		return byName, res.Note
	}

	// The node's list holds read_file and tool_search; read_file is
	// catalogue-only on the deferring fixture. Same query both sides.
	t.Run("defer on marks the catalogue-only match program_only without its schema", func(t *testing.T) {
		r := newRegistry(t)
		ctx := WithNodeExecutableTools(context.Background(), []string{"read_file", "tool_search"})
		ctx = WithCatalogueOnlyTools(ctx, []string{"read_file"})
		matches, note := search(t, r, ctx, "select:read_file")
		m, ok := matches["read_file"]
		if !ok {
			t.Fatal("select:read_file found no read_file entry")
		}
		if !m.Available {
			t.Error("a catalogue-only tool answered unavailable — the door still runs it, from a program")
		}
		if !m.ProgramOnly {
			t.Error("the pinned catalogue-only match is not marked program_only — the model reads it as directly callable")
		}
		if len(m.InputSchema) != 0 {
			t.Errorf("a program_only match handed out its schema — the deferral's bytes are re-spent here: %s", m.InputSchema)
		}
		if !strings.Contains(note, "program_only") || !strings.Contains(note, "call.describe") {
			t.Errorf("the note does not say how a program-only match is reached: %q", note)
		}
		// The dispatcher's map is the union: a direct call to a deferred
		// tool still executes, so the note must not claim otherwise.
		if strings.Contains(note, "never directly") || strings.Contains(note, "no direct call") {
			t.Errorf("the note claims a direct call is impossible, which the dispatcher contradicts: %q", note)
		}
	})

	t.Run("defer off keeps the direct-call wording and the schema", func(t *testing.T) {
		r := newRegistry(t)
		ctx := WithNodeExecutableTools(context.Background(), []string{"read_file", "tool_search"})
		matches, note := search(t, r, ctx, "select:read_file")
		m, ok := matches["read_file"]
		if !ok {
			t.Fatal("select:read_file found no read_file entry")
		}
		if !m.Available || m.ProgramOnly {
			t.Errorf("available=%v program_only=%v, want the plain available verdict", m.Available, m.ProgramOnly)
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(m.InputSchema, &schema); err != nil || len(schema.Properties) == 0 {
			t.Errorf("the undeferred match must keep the schema: %s", m.InputSchema)
		}
		if strings.Contains(note, "program_only") {
			t.Errorf("a node without the split gained the deferral note: %q", note)
		}
	})
}
