package model

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/scripthost/toolindex"
)

// The seam redacts a tool definition's model-facing text. A definition's
// description and input schema are, for an MCP tool, a REMOTE server's text,
// and they reach a model three ways downstream of the seam: the wire schema
// buildRequest encodes for the request, the codemode catalogue and its
// description index, and the IOTask wire to a sandboxed runner (whose
// in-container model reads them verbatim). These tests hold all three
// readers against one guard, and hold the dispatch keys — the tool's name
// and the schema's property names — as written.

func TestAToolDefinitionReachesTheModelAsItsPlaceholder(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-9c2e7a1b3d4f-tooldef"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}

	mcpSchema := `{` + `
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "the page to scrape",
				"default": "` + sentinel + `",
				"enum": ["` + sentinel + `", "everything"]
			},
			"opts": {
				"type": "object",
				"description": "nested options whose documentation quotes ` + sentinel + `"
			}
		}
	}`
	// Odd spacing and a 20-digit integer: a value the guard does not know
	// must leave the document byte for byte — no re-encoding, no precision
	// lost in a decode/re-encode round trip.
	cleanSchema := `{ "type":  "object",  "properties": {  "count":  {  "type": "integer",  "default": 12345678901234567890  }  }  }`
	in := delegate.Task{
		ToolDefs: []delegate.ToolDef{
			{
				Name:          "mcp_firecrawl.scrape",
				QualifiedName: "mcp.firecrawl.scrape",
				Description:   "Scrape one page. The server echoes its key: " + sentinel,
				InputSchema:   json.RawMessage(mcpSchema),
			},
			{
				Name:        "read_file",
				Description: "Read a file from the workspace",
				InputSchema: json.RawMessage(cleanSchema),
			},
		},
	}
	out := e.taskForModel(in)

	// The boundary copies: the executor's own task — what the events, the
	// logs and the session ledger read — keeps the definitions as resolved.
	if !strings.Contains(in.ToolDefs[0].Description, sentinel) || !bytes.Contains(in.ToolDefs[0].InputSchema, []byte(sentinel)) {
		t.Fatal("the boundary mutated the caller's tool definitions in place")
	}

	// A definition holding no known value keeps its schema byte-identical.
	if !bytes.Equal(out.ToolDefs[1].InputSchema, in.ToolDefs[1].InputSchema) {
		t.Errorf("a definition holding no known value lost its schema bytes: got %s", out.ToolDefs[1].InputSchema)
	}

	// The wire: what buildRequest sends for the screened definitions.
	opts := GenerationOptions{Model: "anthropic/claude-opus-5"}
	opts.Tools = toolDefsToGeneration(out.ToolDefs)
	req, err := buildRequest(opts, nil, nil, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	wire, err := json.Marshal(req.Tools)
	if err != nil {
		t.Fatalf("marshal the wire tools: %v", err)
	}
	if strings.Contains(string(wire), sentinel) {
		t.Errorf("the wire tool schema carries the known value: %s", wire)
	}
	if n := strings.Count(string(wire), placeholder); n != 4 {
		t.Errorf("the wire carries %d placeholders, want 4 (the description, the default, the enum member, the nested description): %s", n, wire)
	}
	if !strings.Contains(string(wire), `"name":"mcp_firecrawl.scrape"`) || !strings.Contains(string(wire), `"query"`) {
		t.Errorf("the wire lost a dispatch key — the tool's or the property's name: %s", wire)
	}

	// The IOTask wire to a sandboxed runner: the in-container model reads
	// what this JSON carries.
	ioJSON, err := json.Marshal(delegate.ToIOTask(out))
	if err != nil {
		t.Fatalf("marshal the IOTask: %v", err)
	}
	if strings.Contains(string(ioJSON), sentinel) {
		t.Errorf("the IOTask wire carries the known value: %s", ioJSON)
	}

	// Without a guard the seam is the identity on the definitions.
	bare := (&ClawExecutor{}).taskForModel(in)
	if bare.ToolDefs[0].Description != in.ToolDefs[0].Description ||
		!bytes.Equal(bare.ToolDefs[0].InputSchema, in.ToolDefs[0].InputSchema) {
		t.Error("a guard-less executor rewrote the tool definitions")
	}
	// With placeholders off the pass renders values by the operator's
	// choice, and the definitions read exactly as they came in.
	switchOff := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}},
		func() secretguard.Config { cfg := secretguard.DefaultConfig(); cfg.Placeholders = false; return cfg }())
	plain := (&ClawExecutor{secretGuard: switchOff}).taskForModel(in)
	if plain.ToolDefs[0].Description != in.ToolDefs[0].Description ||
		!bytes.Equal(plain.ToolDefs[0].InputSchema, in.ToolDefs[0].InputSchema) {
		t.Error("with placeholders off the seam still rewrote the tool definitions")
	}
}

// The codemode index composes a description's first 120 runes into the text
// a program reads. A value the seam leaves whole is therefore CUT IN TWO by
// the index — no pass at or below the wire could recognise it any more. The
// screen at the seam is what keeps the value out of the index: the premise
// pins the cut on the unscreened definitions, and the assertion holds the
// screened index on the very prefix of the value the cut carries.
func TestTheCodemodeIndexIsComposedFromScreenedDefinitions(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-9c2e7a1b3d4f-tooldefindex"
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}

	defs := []delegate.ToolDef{{
		Name:        "mcp_firecrawl.scrape",
		Description: strings.Repeat("a", 100) + sentinel,
	}}

	index := func(defs []delegate.ToolDef) string {
		catalog := scriptCatalog(toolDefsToGeneration(defs), ScriptHostToolName)
		bounded, omitted, err := toolindex.Bound(catalog)
		if err != nil {
			t.Fatalf("bound the catalogue: %v", err)
		}
		return codemodeToolDescription(bounded, omitted, false)
	}

	// Premise: the index built from the UNSCREENED definitions carries the
	// head of the value the 120-rune summary keeps, not the whole of it.
	raw := index(defs)
	if !strings.Contains(raw, sentinel[:10]) {
		t.Fatalf("the premise moved: the 120-rune summary no longer keeps the head of the value: %q", raw)
	}
	if strings.Contains(raw, sentinel) {
		t.Fatalf("the premise moved: the index carries the whole value, so the seam is not what the index needs")
	}

	screened := index(e.taskForModel(delegate.Task{ToolDefs: defs}).ToolDefs)
	if strings.Contains(screened, sentinel[:10]) {
		t.Errorf("the codemode index built from the screened definitions still carries the head of the value: %q", screened)
	}
}

// An undecodable schema is handed back as is — RedactJSON cannot decode it —
// and no model ever reads it: buildRequest refuses the request rather than
// encoding the document, and the codemode index omits the schema rather than
// describing it. That refusal pair is what makes returning the raw bytes
// safe instead of a silent fallback.
func TestAnUndecodableSchemaIsReturnedAsIsAndNeverServed(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-9c2e7a1b3d4f-tooldef"
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}

	// Truncated JSON: undecodable for the seam's RedactJSON, for the wire's
	// schema decoder and for the catalogue alike.
	garbage := `{"type":"object","properties":{"q":{"description":"` + sentinel + `"`
	defs := []delegate.ToolDef{{
		Name:        "mcp.srv.broken",
		Description: "read " + sentinel,
		InputSchema: json.RawMessage(garbage),
	}}
	out := e.taskForModel(delegate.Task{ToolDefs: defs})
	if !bytes.Equal(out.ToolDefs[0].InputSchema, []byte(garbage)) {
		t.Fatalf("an undecodable schema was rewritten: %s", out.ToolDefs[0].InputSchema)
	}

	// The wire refuses it, naming the tool.
	_, err := buildRequest(GenerationOptions{Model: "anthropic/claude-opus-5", Tools: toolDefsToGeneration(out.ToolDefs)}, nil, nil, nil)
	if err == nil {
		t.Fatal("buildRequest encoded an undecodable schema instead of refusing the request")
	}
	if !strings.Contains(err.Error(), "mcp.srv.broken") {
		t.Errorf("the refusal does not name the tool whose schema it cannot decode: %v", err)
	}

	// The catalogue omits it and records how large it was.
	catalog := scriptCatalog(toolDefsToGeneration(out.ToolDefs), ScriptHostToolName)
	bounded, _, err := toolindex.Bound(catalog)
	if err != nil {
		t.Fatalf("bound the catalogue: %v", err)
	}
	if bounded[0].SchemaOmitted == 0 || bounded[0].InputSchema != nil {
		t.Errorf("the catalogue served an undecodable schema: omitted=%d schema=%s", bounded[0].SchemaOmitted, bounded[0].InputSchema)
	}
}

// A schema property's NAME is a dispatch key: the model sends it back as the
// argument key, and materialisation never rewrites keys. So a property whose
// name contains a declared value keeps that name byte for byte on the wire
// and on the IOTask, while the same value in the property's description is
// screened. The schema lists no `required` member: a required entry naming
// that property is a string value, and the pass screens it.
func TestASchemaPropertyNameHoldingAKnownValueStaysAsWritten(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-9c2e7a1b3d4f-tooldefkey"
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}

	key := "x_" + sentinel
	schema := `{"type":"object","properties":{"` + key + `":{"type":"string","description":"` + sentinel + `"}}}`
	out := e.taskForModel(delegate.Task{ToolDefs: []delegate.ToolDef{{
		Name:        "mcp_srv.lookup",
		Description: "Look a record up",
		InputSchema: json.RawMessage(schema),
	}}})

	req, err := buildRequest(GenerationOptions{Model: "anthropic/claude-opus-5", Tools: toolDefsToGeneration(out.ToolDefs)}, nil, nil, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	wire, err := json.Marshal(req.Tools)
	if err != nil {
		t.Fatalf("marshal the wire tools: %v", err)
	}
	ioJSON, err := json.Marshal(delegate.ToIOTask(out))
	if err != nil {
		t.Fatalf("marshal the IOTask: %v", err)
	}
	for _, c := range []struct{ reader, text string }{{"wire", string(wire)}, {"IOTask", string(ioJSON)}} {
		if !strings.Contains(c.text, `"`+key+`":{`) {
			t.Errorf("the %s renamed the property key %q: %s", c.reader, key, c.text)
		}
		if strings.Contains(c.text, `"description":"`+sentinel) {
			t.Errorf("the %s carries the known value in the property's description: %s", c.reader, c.text)
		}
	}
}

// End to end through the real executor and claw backend: an MCP tool whose
// remote description, enum member and property description echo a declared
// secret. Every request the model receives carries only the placeholder,
// and the model's call with the enum member's placeholder still reaches the
// tool as the real value.
func TestAnMCPToolDefinitionReachesTheModelScreenedThroughTheExecutor(t *testing.T) {
	t.Parallel()
	const key = "AIzaSyD-tooldef-9f8e7d6c5b4a3210"
	placeholder := secretguard.PlaceholderForName("MAPS_KEY")
	guard := secretguard.New([]secretguard.Secret{{Name: "MAPS_KEY", Value: key}}, secretguard.DefaultConfig())

	tools := tool.NewRegistry()
	ranWith := make(chan string, 1)
	schema := `{"type":"object","properties":{"q":{"type":"string","enum":["` + key + `","other"],"description":"pass ` + key + `"}},"required":["q"]}`
	if err := tools.RegisterMCP("srv", "scrape", "Scrape with key "+key, json.RawMessage(schema), func(_ context.Context, in json.RawMessage) (string, error) {
		ranWith <- string(in)
		return "ok", nil
	}); err != nil {
		t.Fatalf("register the MCP tool: %v", err)
	}
	// The executor appends todo_write to a claw node with a non-empty tool
	// list, so the registry must resolve it.
	if err := tools.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatalf("register todo_write: %v", err)
	}

	reg := NewRegistry()
	mock := newMockClient(
		toolUseEvents("t1", "mcp_srv_scrape", `{"q":"`+placeholder+`"}`, 50, 10),
		textEvents("done", 60, 5),
	)
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Look it up."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tools), WithSecretGuard(guard))
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n"},
		LLMFields:        ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:            []string{"mcp.srv.scrape"},
		ActiveMCPServers: []string{"srv"},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("execute the node: %v", err)
	}

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("the model received %d requests, want 2 (the tool call, then the answer)", len(calls))
	}
	for i, c := range calls {
		wire, err := json.Marshal(c.Tools)
		if err != nil {
			t.Fatalf("marshal the tools of request %d: %v", i, err)
		}
		if strings.Contains(string(wire), key) {
			t.Errorf("request %d hands the model a tool definition carrying the known value: %s", i, wire)
		}
		if !strings.Contains(string(wire), placeholder) {
			t.Errorf("request %d lost the MCP tool's screened definition: %s", i, wire)
		}
	}
	select {
	case got := <-ranWith:
		if !strings.Contains(got, key) {
			t.Errorf("the MCP tool ran with %s, want the enum placeholder materialised to the value", got)
		}
	default:
		t.Fatal("the MCP tool never ran")
	}
}
