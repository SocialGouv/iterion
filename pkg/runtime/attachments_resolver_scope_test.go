package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2864 — resolveRef had no attachments arm, so {{attachments.*}} rendered
// empty on every surface that resolves through it (an edge `with:`, an emit
// payload, a fail `message:`, a human node's instructions) while prompts and
// tool bodies resolved fine through the model template resolver. One witness
// per surface, all reading the same run attachment.

// attachmentScope builds an engine whose run state holds one attachment,
// with every sub-field the namespace exposes.
func attachmentScope(t *testing.T) (*Engine, *runState) {
	t.Helper()
	wf := &ir.Workflow{
		Name:  "att_scope",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
		},
	}
	e := New(wf, tmpStore(t), newStubExecutor(), WithLogger(iterlog.Nop()))
	rs := e.newRunState("run-att-scope", nil)
	rs.attachments = map[string]model.AttachmentInfo{
		"spec": {
			Name:   "spec",
			Path:   "/att/spec/spec.md",
			MIME:   "text/markdown",
			Size:   4096,
			SHA256: "abc123",
			PresignURL: func() (string, error) {
				return "https://x/spec?sig=1", nil
			},
		},
	}
	return e, rs
}

// The sub-field rule is the model resolver's: path by default, then url,
// mime, size, sha256; an unknown name or sub-field resolves to nil.
func TestAttachmentsRefSubFieldsThroughResolveRef(t *testing.T) {
	e, rs := attachmentScope(t)
	sc := rs.scope()
	for _, tc := range []struct {
		text string
		want any
	}{
		{"{{attachments.spec}}", "/att/spec/spec.md"},
		{"{{attachments.spec.path}}", "/att/spec/spec.md"},
		{"{{attachments.spec.url}}", "https://x/spec?sig=1"},
		{"{{attachments.spec.mime}}", "text/markdown"},
		{"{{attachments.spec.size}}", int64(4096)},
		{"{{attachments.spec.sha256}}", "abc123"},
		{"{{attachments.nope}}", nil},
		{"{{attachments.spec.nope}}", nil},
	} {
		refs := refsOf(t, tc.text)
		if got := e.resolveRef(refs[0], sc); got != tc.want {
			t.Errorf("%s = %v (%T), want %v", tc.text, got, got, tc.want)
		}
	}
}

// An edge `with:` mapping carries the reference — whole (type intact) and
// interpolated into prose alike.
func TestAttachmentsRefTravelsThroughAnEdgeWith(t *testing.T) {
	e, rs := attachmentScope(t)
	rs.outputs["a"] = map[string]any{"ok": true}
	e.workflow.Edges = []*ir.Edge{{
		From: "a", To: "b",
		With: []*ir.DataMapping{
			{Key: "p", Raw: "{{attachments.spec.path}}", Refs: refsOf(t, "{{attachments.spec.path}}")},
			{Key: "desc", Raw: "spec at {{attachments.spec.path}} ({{attachments.spec.size}} b)", Refs: refsOf(t, "spec at {{attachments.spec.path}} ({{attachments.spec.size}} b)")},
		},
	}}
	input := e.buildNodeInputRS("b", rs.scope())
	if input["p"] != "/att/spec/spec.md" {
		t.Errorf("with p = %v, want the attachment path", input["p"])
	}
	if input["desc"] != "spec at /att/spec/spec.md (4096 b)" {
		t.Errorf("with desc = %v, want the interpolated text", input["desc"])
	}
}

// An emit node's payload resolves the reference like any other mapping.
func TestAttachmentsRefTravelsThroughAnEmitPayload(t *testing.T) {
	e, rs := attachmentScope(t)
	en := &ir.EmitNode{
		BaseNode: ir.BaseNode{ID: "pub"},
		Event:    "spec-ready",
		With: []*ir.DataMapping{
			{Key: "path", Raw: "{{attachments.spec.path}}", Refs: refsOf(t, "{{attachments.spec.path}}")},
			{Key: "url", Raw: "{{attachments.spec.url}}", Refs: refsOf(t, "{{attachments.spec.url}}")},
		},
	}
	payload := e.emitEvent(rs, en, rs.scope())
	if payload["path"] != "/att/spec/spec.md" || payload["url"] != "https://x/spec?sig=1" {
		t.Errorf("emit payload = %v, want the attachment's path and url", payload)
	}
}

// A human node's instructions render the reference (and the review
// companion's system prompt, which renders through the same function).
func TestAttachmentsRefRendersInHumanInstructions(t *testing.T) {
	e, rs := attachmentScope(t)
	p := &ir.Prompt{
		Name:         "ask",
		Body:         "Review {{attachments.spec.path}} — {{attachments.spec.url}} — sha {{attachments.spec.sha256}}",
		TemplateRefs: refsOf(t, "Review {{attachments.spec.path}} — {{attachments.spec.url}} — sha {{attachments.spec.sha256}}"),
	}
	text := e.renderHumanInstructions(p, map[string]any{}, rs)
	for _, want := range []string{"/att/spec/spec.md", "https://x/spec?sig=1", "abc123"} {
		if !strings.Contains(text, want) {
			t.Errorf("instructions %q miss %q", text, want)
		}
	}
}

// A fail node's `message:` renders the reference at fail time — the
// end-to-end witness: the attachment is written to the store before the
// run, loaded by runInitState, and read by failOutcome through resolveRef.
func TestAttachmentsRefRendersInAFailMessage(t *testing.T) {
	const src = `dsl: 2

attachments:
  spec: file

agent lead:
  model: "m"

fail stopped:
  code: STOPPED
  message: "spec at {{attachments.spec.path}} ({{attachments.spec.mime}})"

workflow w:
  worktree: none
  entry: lead
  lead -> stopped
`
	cr := compileBot(t, src)
	if cr.HasErrors() {
		t.Fatalf("fixture does not compile: %v", cr.Diagnostics)
	}
	ctx := context.Background()
	s := tmpStore(t)
	runID := "run-att-fail"
	if _, err := s.CreateRun(ctx, runID, "w", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAttachment(ctx, runID, store.AttachmentRecord{Name: "spec", OriginalFilename: "spec.md", MIME: "text/markdown"}, strings.NewReader("# spec")); err != nil {
		t.Fatal(err)
	}
	eng := New(cr.Workflow, s, newStubExecutor(), WithLogger(iterlog.Nop()))
	if err := eng.Run(ctx, runID, nil); err == nil {
		t.Fatal("the run succeeded; the graph routes to a fail node")
	}
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	want := "spec at " + filepath.Join(s.Root(), "runs", runID, "attachments", "spec", "spec.md") + " (text/markdown)"
	if run.Error != want {
		t.Errorf("the operator reads %q, want %q", run.Error, want)
	}
}
