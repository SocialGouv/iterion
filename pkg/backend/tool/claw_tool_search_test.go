package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// A declared tool_search searches the registry's WHOLE catalogue, which is
// wider than the node's tools: list. The answer must say so per match
// (#2717): a tool the node cannot execute is marked unavailable and hands out
// no schema — the reviewer who ran `select:bash` without bash in its list was
// otherwise told "bash exists", called it, and met unknown tool.
func TestClawToolSearch_ReportsWhatThisNodeCanExecute(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltinsWithScope(r, NewReadScope(t.TempDir()), nil); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	snapshot := func() []api.Tool { return registrySnapshot(r) }
	if err := RegisterClawToolSearch(r, snapshot); err != nil {
		t.Fatalf("RegisterClawToolSearch: %v", err)
	}
	td, err := r.Resolve("tool_search")
	if err != nil {
		t.Fatalf("resolve tool_search: %v", err)
	}
	if !hasTool(r, "bash") || !hasTool(r, "read_file") {
		t.Fatalf("the fixture lacks the tools under test")
	}

	// The node's list holds read_file and tool_search — NOT bash.
	ctx := WithNodeExecutableTools(context.Background(), []string{"read_file", "tool_search"})

	type matchDetail struct {
		Name        string          `json:"name"`
		Available   bool            `json:"available"`
		InputSchema json.RawMessage `json:"input_schema"`
		Reason      string          `json:"unavailable_reason"`
	}
	search := func(query string) map[string]matchDetail {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"query": query})
		out, err := td.Execute(ctx, in)
		if err != nil {
			t.Fatalf("tool_search %q: %v", query, err)
		}
		var res struct {
			Tools []matchDetail `json:"tools"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("the per-node answer does not decode (%v) — the legacy shape answered for the catalogue, not the node: %s", err, out)
		}
		byName := make(map[string]matchDetail, len(res.Tools))
		for _, m := range res.Tools {
			byName[m.Name] = m
		}
		return byName
	}

	t.Run("select a tool the node cannot execute", func(t *testing.T) {
		m, ok := search("select:bash")["bash"]
		if !ok {
			t.Fatal("select:bash found no bash entry at all — the catalogue match must stay visible, marked unavailable")
		}
		if m.Available {
			t.Error("bash answered as available to a node without it — the reviewer is still lied to")
		}
		if len(m.InputSchema) != 0 {
			t.Errorf("an unavailable tool handed out its schema — the invitation to call it: %s", m.InputSchema)
		}
		if !strings.Contains(m.Reason, "tools:") {
			t.Errorf("the unavailability carries no reason the model can act on: %q", m.Reason)
		}
	})

	t.Run("select a tool the node CAN execute", func(t *testing.T) {
		m, ok := search("select:read_file")["read_file"]
		if !ok {
			t.Fatal("select:read_file found no read_file entry")
		}
		if !m.Available {
			t.Error("read_file answered as unavailable to a node that declares it")
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(m.InputSchema, &schema); err != nil || len(schema.Properties) == 0 {
			t.Errorf("an available match must carry the schema the select: wording promises: %s", m.InputSchema)
		}
	})

	t.Run("a keyword search marks both sides", func(t *testing.T) {
		matches := search("read")
		if len(matches) == 0 {
			t.Fatal("a keyword search matched nothing in a full registry")
		}
		for name, m := range matches {
			want := name == "read_file"
			if m.Available != want {
				t.Errorf("match %q: available = %v, want %v", name, m.Available, want)
			}
		}
	})
}

// Without the dispatch's pin the answer stays the catalogue's: a caller that
// cannot name the node's set must not guess it.
func TestClawToolSearch_NoScopeKeepsTheLegacyAnswer(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltinsWithScope(r, NewReadScope(t.TempDir()), nil); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	if err := RegisterClawToolSearch(r, func() []api.Tool { return registrySnapshot(r) }); err != nil {
		t.Fatalf("RegisterClawToolSearch: %v", err)
	}
	td, err := r.Resolve("tool_search")
	if err != nil {
		t.Fatalf("resolve tool_search: %v", err)
	}
	in, _ := json.Marshal(map[string]any{"query": "select:bash"})
	out, err := td.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("tool_search: %v", err)
	}
	var legacy struct {
		Matches []string `json:"matches"`
		Tools   []any    `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &legacy); err != nil {
		t.Fatalf("the legacy answer does not decode: %v", err)
	}
	if len(legacy.Matches) != 1 || legacy.Matches[0] != "bash" {
		t.Errorf("legacy matches = %v, want [bash]", legacy.Matches)
	}
	if legacy.Tools != nil {
		t.Errorf("an unscoped answer gained the per-node detail — who pinned the scope? tools = %v", legacy.Tools)
	}
}
