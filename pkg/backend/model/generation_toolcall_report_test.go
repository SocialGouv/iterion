package model

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// toolCallInfoConstructor is the one function allowed to write a
// ToolCallInfo literal.
const toolCallInfoConstructor = "toolCallInfo"

// TestEveryToolCallInfoComesFromTheOneConstructor is the census behind
// toolCallInfo's doc comment: a production site that writes its own
// ToolCallInfo literal is a site where a nested call's parent, or its batch
// slot, goes missing from the run's events — the class this file closes was
// two refusal paths that had left even the call's own id off.
//
// Reads the package's files from disk: an overlay cannot mutate what this
// walk sees, so a mutation of it is a file written, run and removed.
func TestEveryToolCallInfoComesFromTheOneConstructor(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	visited, constructors := 0, 0
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- this package's own files
		if err != nil {
			t.Errorf("%s could not be read, so it was NOT inventoried: %v", name, err)
			continue
		}
		visited++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Errorf("%s could not be parsed, so it was NOT inventoried: %v", name, err)
			continue
		}
		for _, decl := range f.Decls {
			owner := "<package scope>"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "ToolCallInfo" {
					if owner == toolCallInfoConstructor {
						constructors++
					} else {
						offenders = append(offenders, fset.Position(lit.Pos()).String()+" in "+owner)
					}
				}
				return true
			})
		}
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s writes a ToolCallInfo literal: build it with toolCallInfo(tu) and set the outcome fields, so the call's id, parent and batch slot reach its event", o)
	}
	if constructors != 1 {
		t.Errorf("the constructor %s holds %d ToolCallInfo literals, want exactly 1: the census lost its anchor", toolCallInfoConstructor, constructors)
	}
	if visited < toolExecuteWalkFloor {
		t.Errorf("the walk read %d production files, under the floor of %d: it collapsed", visited, toolExecuteWalkFloor)
	}
}

// TestLLMRequestCountsTheToolsTheRequestDeclared: llm_request's tool_count is
// what went on the wire. A deferring node keeps its catalogue-only tools off
// the request, and says how many beside the count rather than inside it.
func TestLLMRequestCountsTheToolsTheRequestDeclared(t *testing.T) {
	t.Parallel()
	for _, deferOn := range []bool{false, true} {
		var requests []store.Event
		emitter := codemodeEventEmitter(func(e store.Event) {
			if e.Type == store.EventLLMRequest {
				requests = append(requests, e)
			}
		})
		reg := NewRegistry()
		cap := &requestCapturingClient{}
		reg.Register("test", func(string) (api.APIClient, error) { return cap, nil })
		backend := NewClawBackend(reg, NewStoreEventHooks(context.Background(), emitter, "run", iterlog.Nop(), nil, nil), RetryPolicy{})
		if _, err := backend.Execute(context.Background(), delegate.Task{
			NodeID: "n", Model: "test/test-model", UserPrompt: "go", HasTools: true,
			ToolDefs: deferredToolDefs(), Codemode: true, CodemodeDefer: deferOn,
		}); err != nil {
			t.Fatalf("defer=%v: Execute: %v", deferOn, err)
		}
		if len(requests) == 0 || len(requests) != len(cap.requests) {
			t.Fatalf("defer=%v: %d llm_request events for %d requests", deferOn, len(requests), len(cap.requests))
		}
		// deferredToolDefs: read_file and grep demote under defer; ask_user,
		// todo_write and codemode stay declared.
		wantCatalogue := 0
		if deferOn {
			wantCatalogue = 2
		}
		for i, e := range requests {
			declared := len(cap.requests[i].Tools)
			if e.Data["tool_count"] != declared {
				t.Errorf("defer=%v: tool_count = %v, the request declared %d tools", deferOn, e.Data["tool_count"], declared)
			}
			got, present := e.Data["catalogue_only_count"]
			if present != (wantCatalogue > 0) || (present && got != wantCatalogue) {
				t.Errorf("defer=%v: catalogue_only_count = %v (present %v), want %d, omitted when zero", deferOn, got, present, wantCatalogue)
			}
		}
	}
}

// structuredAnswerClient answers every request with a call to the synthetic
// structured-output tool, and keeps the requests it was sent.
type structuredAnswerClient struct{ requests []api.CreateMessageRequest }

func (c *structuredAnswerClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.requests = append(c.requests, req)
	ch := make(chan api.StreamEvent, 8)
	ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 10}
	ch <- api.StreamEvent{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "structured_output"}}
	ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":true}`}}
	ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
	ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 5}}
	ch <- api.StreamEvent{Type: api.EventMessageStop}
	close(ch)
	return ch, nil
}

// TestANodeWithNoToolsCountsNoneOnItsStructuredRequest: the synthetic
// structured-output tool is on the wire but is not one of the node's, so a
// tools-less node with an output schema reports tool_count 0 — the
// signature the review-pr bundle names a tools-less dispatch by.
func TestANodeWithNoToolsCountsNoneOnItsStructuredRequest(t *testing.T) {
	t.Parallel()
	var requests []store.Event
	emitter := codemodeEventEmitter(func(e store.Event) {
		if e.Type == store.EventLLMRequest {
			requests = append(requests, e)
		}
	})
	reg := NewRegistry()
	client := &structuredAnswerClient{}
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	schema, err := SchemaToJSON(&ir.Schema{Name: "verdict", Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}}})
	if err != nil {
		t.Fatal(err)
	}
	backend := NewClawBackend(reg, NewStoreEventHooks(context.Background(), emitter, "run", iterlog.Nop(), nil, nil), RetryPolicy{})
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "judge", Model: "test/test-model", UserPrompt: "judge", OutputSchema: schema,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(client.requests) == 0 || len(client.requests[0].Tools) != 1 {
		t.Fatalf("the structured request carried %d tools, want the synthetic one alone: the fixture is not the case under test", len(client.requests[0].Tools))
	}
	if len(requests) != len(client.requests) {
		t.Fatalf("%d llm_request events for %d requests", len(requests), len(client.requests))
	}
	for _, e := range requests {
		if e.Data["tool_count"] != 0 {
			t.Errorf("tool_count = %v on a node with no tools, want 0: the synthetic structured-output tool is not the node's", e.Data["tool_count"])
		}
		if _, present := e.Data["catalogue_only_count"]; present {
			t.Errorf("catalogue_only_count is present on a node that defers nothing: %v", e.Data)
		}
	}
}
