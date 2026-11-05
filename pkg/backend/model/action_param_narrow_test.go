package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// bodyRecorder is a vendor that keeps the JSON it received for `body`, the
// oracle of every narrowing case: what reaches the wire, not what the executor
// reports.
type bodyRecorder struct {
	mu    sync.Mutex
	got   []string
	calls int
}

func (b *bodyRecorder) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(raw, &payload)
	b.mu.Lock()
	b.got = append(b.got, string(payload["body"]))
	b.calls++
	b.mu.Unlock()
	w.WriteHeader(201)
	_, _ = w.Write([]byte(`{}`))
}

func (b *bodyRecorder) last(t *testing.T) (string, int) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.got) == 0 {
		return "", b.calls
	}
	return b.got[len(b.got)-1], b.calls
}

// narrowingNode runs `body: <value>` against an operation declaring `body`
// with the given type and items, through the public Execute path.
func narrowingNode(t *testing.T, declared, items, value string, opts ...model.ClawExecutorOption) (*bodyRecorder, func(map[string]any) error) {
	t.Helper()
	rec := &bodyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(srv.Close)

	pkg, op := mutatingPackage(srv.URL)
	op.Params[0].Type = declared
	op.Params[0].Items = items
	pkg.Ops[0].Operations[0] = op

	refs, err := ir.ParseRefs(value)
	if err != nil {
		t.Fatalf("ParseRefs(%q): %v", value, err)
	}
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{Key: "body", Value: value, Refs: refs}},
	}
	opts = append(opts, model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{}, opts...)
	return rec, func(input map[string]any) error {
		_, err := e.Execute(context.Background(), node, input)
		return err
	}
}

// Every declared type narrows the same way: text by the declaration, a typed
// value when it is of the declared type, an array's elements to its items —
// the structured types included, which read a text as the JSON it spells
// exactly as a scalar reads its spelling.
//
// Mutations seen red (each restored): the text branch of the `array` case
// removed (the literal list refused); the `items` loop skipped (`["1","2"]`
// reached an integer list as strings); the text branch of the `object` case
// removed (the literal object refused).
func TestEveryDeclaredTypeNarrowsAWholeValueAndATextTheSameWay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		declared, items string
		value           string
		input           map[string]any
		want            string
	}{
		{name: "a literal JSON list reaches an array", declared: "array", value: `["a11y","rgaa"]`, want: `["a11y","rgaa"]`},
		{name: "a whole-value list reaches an array", declared: "array", value: "{{input.labels}}", input: map[string]any{"labels": []any{"a11y", "rgaa"}}, want: `["a11y","rgaa"]`},
		{name: "a string holding a JSON list reaches an array", declared: "array", value: "{{input.labels}}", input: map[string]any{"labels": `["a11y"]`}, want: `["a11y"]`},
		{name: "list elements narrow to the declared items", declared: "array", items: "integer", value: "{{input.ids}}", input: map[string]any{"ids": []any{"1", "2"}}, want: `[1,2]`},
		{name: "a large integer element keeps every digit", declared: "array", items: "integer", value: `[9007199254740993]`, want: `[9007199254740993]`},
		{name: "a literal JSON object reaches an object", declared: "object", value: `{"state":"open"}`, want: `{"state":"open"}`},
		{name: "a whole-value object reaches an object", declared: "object", value: "{{input.doc}}", input: map[string]any{"doc": map[string]any{"k": 1}}, want: `{"k":1}`},
		{name: "a literal integer reaches an integer", declared: "integer", value: "42", want: `42`},
		{name: "a whole-value numeral string reaches an integer", declared: "integer", value: "{{input.n}}", input: map[string]any{"n": "42"}, want: `42`},
		{name: "a literal number reaches a number", declared: "number", value: "1.5", want: `1.5`},
		{name: "a literal boolean reaches a boolean", declared: "boolean", value: "true", want: `true`},
		{name: "a whole-value boolean reaches a boolean", declared: "boolean", value: "{{input.b}}", input: map[string]any{"b": false}, want: `false`},
		{name: "a whole-value number reaches a string as its digits", declared: "string", value: "{{input.n}}", input: map[string]any{"n": 7}, want: `"7"`},
		{name: "an untyped declaration takes the value as it is", declared: "", value: "{{input.doc}}", input: map[string]any{"doc": map[string]any{"k": "v"}}, want: `{"k":"v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, run := narrowingNode(t, tc.declared, tc.items, tc.value)
			if err := run(tc.input); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got, _ := rec.last(t); got != tc.want {
				t.Errorf("the vendor received %s, want %s", got, tc.want)
			}
		})
	}
}

// A value the declaration cannot describe is refused locally, as a typed
// *model.ActionParamError naming the argument, the element and the declared
// type — and the vendor is never called.
//
// Mutations seen red (each restored): the `object` case returning its value
// unchanged (a string, an array and a number reached the vendor); the default
// arm returning the value (a `file` and an unknown type reached it); the
// refusal flattened to fmt.Errorf("%v") in renderActionParams (errors.As found
// no *ActionParamError); the element index dropped from the refusal (Item -1).
func TestAValueTheDeclarationCannotDescribeIsATypedRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		declared, items string
		value           string
		input           map[string]any
		wantItem        int
		wantDeclared    string
		unsupported     bool
	}{
		{name: "a comma list is not a JSON array", declared: "array", value: "a11y,rgaa", wantItem: -1, wantDeclared: "array"},
		{name: "a number is not an array", declared: "array", value: "{{input.n}}", input: map[string]any{"n": 3}, wantItem: -1, wantDeclared: "array"},
		{name: "an element that is not an integer", declared: "array", items: "integer", value: `[1,"x"]`, wantItem: 1, wantDeclared: "integer"},
		{name: "an element that is not an object", declared: "array", items: "object", value: `[{"a":1},"b"]`, wantItem: 1, wantDeclared: "object"},
		{name: "plain text is not an object", declared: "object", value: "open", wantItem: -1, wantDeclared: "object"},
		{name: "a string holding a list is not an object", declared: "object", value: "{{input.s}}", input: map[string]any{"s": `["a"]`}, wantItem: -1, wantDeclared: "object"},
		{name: "a list is not an object", declared: "object", value: "{{input.xs}}", input: map[string]any{"xs": []any{"a"}}, wantItem: -1, wantDeclared: "object"},
		{name: "a number is not an object", declared: "object", value: "{{input.n}}", input: map[string]any{"n": 3}, wantItem: -1, wantDeclared: "object"},
		{name: "an object is not a string", declared: "string", value: "{{input.doc}}", input: map[string]any{"doc": map[string]any{"k": 1}}, wantItem: -1, wantDeclared: "string"},
		{name: "a fraction is not an integer", declared: "integer", value: "1.5", wantItem: -1, wantDeclared: "integer"},
		{name: "a word is not a boolean", declared: "boolean", value: "{{input.w}}", input: map[string]any{"w": "maybe"}, wantItem: -1, wantDeclared: "boolean"},
		{name: "a file has no conversion", declared: "file", value: "/etc/hosts", wantItem: -1, wantDeclared: "file", unsupported: true},
		{name: "an unknown type has no conversion", declared: "frobnicate", value: "{{input.n}}", input: map[string]any{"n": 3}, wantItem: -1, wantDeclared: "frobnicate", unsupported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, run := narrowingNode(t, tc.declared, tc.items, tc.value)
			err := run(tc.input)
			if err == nil {
				t.Fatal("the node must refuse a value the declaration cannot describe")
			}
			var refused *model.ActionParamError
			if !errors.As(err, &refused) {
				t.Fatalf("the refusal must be a typed *model.ActionParamError, got %T: %v", err, err)
			}
			if refused.Key != "body" || refused.Item != tc.wantItem || refused.Declared != tc.wantDeclared || refused.Unsupported != tc.unsupported {
				t.Errorf("refusal = {Key:%q Item:%d Declared:%q Unsupported:%v}, want {Key:\"body\" Item:%d Declared:%q Unsupported:%v}",
					refused.Key, refused.Item, refused.Declared, refused.Unsupported, tc.wantItem, tc.wantDeclared, tc.unsupported)
			}
			if !strings.Contains(err.Error(), `param "body"`) {
				t.Errorf("the message must name the argument: %v", err)
			}
			if _, calls := rec.last(t); calls != 0 {
				t.Errorf("the vendor was called %d time(s) with a value the declaration refuses", calls)
			}
		})
	}
}

// A secret materialised into an argument whose declared type refuses it is
// quoted by the refusal — and leaves redacted, with the refusal still typed:
// the message pass keeps errors.As answering, and the refusal's Text, the
// field errors.As hands a caller, is redacted in place.
//
// Mutation seen red (restored): the in-place redaction of ActionParamError.Text
// removed — the message read clean, the field errors.As returned carried the
// secret.
func TestARefusedSecretArgumentIsRedactedAndStaysTyped(t *testing.T) {
	t.Parallel()
	const secretValue = "gh-pat-not-a-real-token-41d7"
	guard := secretguard.New([]secretguard.Secret{{Name: "MY_SECRET", Value: secretValue}}, secretguard.DefaultConfig())
	rec, run := narrowingNode(t, "integer", "", "{{secrets.MY_SECRET}}", model.WithSecretGuard(guard))
	err := run(nil)
	if err == nil {
		t.Fatal("a secret that is not an integer must be refused by an integer argument")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("the refusal carried the secret verbatim: %v", err)
	}
	if !strings.Contains(err.Error(), "__ITERION_SECRET_MY_SECRET__") {
		t.Errorf("the redaction must name the secret it replaced: %v", err)
	}
	var refused *model.ActionParamError
	if !errors.As(err, &refused) {
		t.Fatalf("the redaction must keep the refusal typed, got %T: %v", err, err)
	}
	if refused.Text != "__ITERION_SECRET_MY_SECRET__" {
		t.Errorf("the refusal's Text = %q, want the secret's placeholder — a caller reading the field reads what the message hides", refused.Text)
	}
	if _, calls := rec.last(t); calls != 0 {
		t.Errorf("the vendor was called %d time(s)", calls)
	}
}

// A refusal quotes its text cut to a bound, and a secret the cut halves is no
// longer one the message's redaction recognises: the quoted text is redacted
// BEFORE it is cut, so no part of the secret reaches the message.
//
// Mutation seen red (restored): the in-place redaction of ActionParamError.Text
// removed — the message quoted the secret's first ten bytes.
func TestARefusalCutAcrossASecretQuotesNoPartOfIt(t *testing.T) {
	t.Parallel()
	const secretValue = "gh-pat-not-a-real-token-41d7"
	guard := secretguard.New([]secretguard.Secret{{Name: "MY_SECRET", Value: secretValue}}, secretguard.DefaultConfig())
	// 110 runes ahead of the secret: the 120-rune quote ends ten bytes into it.
	_, run := narrowingNode(t, "integer", "", strings.Repeat("x", 110)+"{{secrets.MY_SECRET}}", model.WithSecretGuard(guard))
	err := run(nil)
	var refused *model.ActionParamError
	if !errors.As(err, &refused) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("the quote must be cut for this case to probe the cut: %v", err)
	}
	if strings.Contains(err.Error(), secretValue[:10]) {
		t.Errorf("the message quotes part of the secret: %v", err)
	}
}

// A launch's TEXT seeded through SetLaunchVars is read at each var's declared
// type — the reading the engine gives the same text — and a name that is no
// var of the workflow is dropped. This is the seed a whole-value
// `{{vars.labels}}` reads when no engine push precedes the node (#2991).
//
// Mutations seen red (each restored): SetLaunchVars storing the raw text (the
// `string[]` var reached the array param as a string and was refused; the
// dotted json drill found nothing); the declared-name check removed (the
// undeclared `zz` resolved).
func TestLaunchTextIsSeededAtEachVarsDeclaredType(t *testing.T) {
	t.Parallel()
	rec := &bodyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.serve))
	defer srv.Close()
	pkg, op := mutatingPackage(srv.URL)
	op.Params[0].Type = "array"
	op.Params[0].Items = "string"
	pkg.Ops[0].Operations[0] = op

	wf := &ir.Workflow{Vars: map[string]*ir.Var{
		"labels": {Name: "labels", Type: ir.VarStringArray},
		"cfg":    {Name: "cfg", Type: ir.VarJSON},
	}}
	e := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
	e.SetLaunchVars(map[string]any{
		"labels": "a11y, rgaa",
		"cfg":    `{"backend":"claw"}`,
		"zz":     "undeclared",
	})

	refs, err := ir.ParseRefs("{{vars.labels}}")
	if err != nil {
		t.Fatal(err)
	}
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "tag"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{Key: "body", Value: "{{vars.labels}}", Refs: refs}},
	}
	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("a string[] var must reach an array param as a list: %v", err)
	}
	if got, _ := rec.last(t); got != `["a11y","rgaa"]` {
		t.Errorf("the vendor received %s, want [\"a11y\",\"rgaa\"]", got)
	}
	if got := e.ResolveRoutingField("{{vars.cfg.backend}}"); got != "claw" {
		t.Errorf("a json var's dotted field resolved to %q, want %q — the seed holds its text, not its document", got, "claw")
	}
	if got := e.ResolveRoutingField("{{vars.zz}}"); got == "undeclared" {
		t.Error("a launch value naming no var of the workflow entered the executor's vars")
	}
}

// A refusal quotes the text it could not read, bounded: the message reaches
// the run's events and logs, and the text may be a whole document. The field
// keeps the text in full for a caller that reads it.
//
// Mutation seen red (restored): quoteBounded quoting the whole text.
func TestARefusalQuotesABoundedText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a11y ", 200)
	_, run := narrowingNode(t, "object", "", "{{input.doc}}")
	err := run(map[string]any{"doc": long})
	var refused *model.ActionParamError
	if !errors.As(err, &refused) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if refused.Text != long {
		t.Errorf("the refusal's Text holds %d bytes, want the %d-byte text in full", len(refused.Text), len(long))
	}
	if n := len(err.Error()); n >= len(long) {
		t.Errorf("the message quotes %d bytes for a %d-byte text: the quote is not bounded", n, len(long))
	}
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("a cut quote must say it is cut: %v", err)
	}
}
