package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// #2086. A node that names an attachment's path runs with a file or not at
// all: rendered empty, the reference sends a tool to `cat ""` and tells an
// agent a file is at "", and both improvise. The admission is at the one
// boundary every execution path crosses, so the fan-out, the router and the
// resume paths are covered by the trunk's witness.

func refsOf(t *testing.T, text string) []*ir.Ref {
	t.Helper()
	refs, err := ir.ParseRefs(text)
	if err != nil {
		t.Fatalf("parse refs of %q: %v", text, err)
	}
	return refs
}

func TestANodeNamingAnAttachmentPathRunsWithAFileOrNotAtAll(t *testing.T) {
	held := func(paths map[string]string) *model.TemplateData {
		td := &model.TemplateData{Attachments: map[string]model.AttachmentInfo{}}
		for name, p := range paths {
			td.Attachments[name] = model.AttachmentInfo{Name: name, Path: p}
		}
		return td
	}
	tool := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "scan"},
		Command:     "cat {{attachments.spec.path}} {{attachments.logo}} {{attachments.spec.url}}",
		CommandRefs: refsOf(t, "cat {{attachments.spec.path}} {{attachments.logo}} {{attachments.spec.url}}"),
	}

	t.Run("a held attachment with no path refuses the node, typed, naming it", func(t *testing.T) {
		err := admitAttachmentPaths(tool, nil, held(map[string]string{"spec": "", "logo": "/run/iterion/attachments/logo/logo.png"}))
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentPathUnavailable {
			t.Fatalf("err = %v, want code %s", err, ErrCodeAttachmentPathUnavailable)
		}
		if rt.NodeID != "scan" {
			t.Fatalf("the refusal names node %q, want scan", rt.NodeID)
		}
		if !strings.Contains(rt.Message, "attachment spec:") || strings.Contains(rt.Message, "logo") {
			t.Fatalf("the refusal names the wrong attachments: %s", rt.Message)
		}
		if !strings.Contains(rt.Hint, ".url") {
			t.Fatalf("the remedy does not name the url accessor: %s", rt.Hint)
		}
	})
	t.Run("a path for every named attachment admits the node", func(t *testing.T) {
		if err := admitAttachmentPaths(tool, nil, held(map[string]string{"spec": "/run/iterion/attachments/spec/spec.md", "logo": "/x/logo.png"})); err != nil {
			t.Fatalf("admitted node refused: %v", err)
		}
	})
	t.Run("an attachment the run does not hold is not this guard's business", func(t *testing.T) {
		if err := admitAttachmentPaths(tool, nil, held(map[string]string{"logo": "/x/logo.png"})); err != nil {
			t.Fatalf("a node naming an absent attachment was refused: %v", err)
		}
	})
	t.Run("a url reference alone needs no path", func(t *testing.T) {
		urlOnly := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "fetch"}, CommandRefs: refsOf(t, "curl {{attachments.spec.url}}")}
		if err := admitAttachmentPaths(urlOnly, nil, held(map[string]string{"spec": ""})); err != nil {
			t.Fatalf("a url-only node was refused: %v", err)
		}
	})
	t.Run("a prompt's reference is the agent node's", func(t *testing.T) {
		w := &ir.Workflow{Prompts: map[string]*ir.Prompt{
			"brief": {Name: "brief", Body: "read {{attachments.spec}}", TemplateRefs: refsOf(t, "read {{attachments.spec}}")},
		}}
		agent := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "ask"}}
		agent.UserPrompt = "brief"
		err := admitAttachmentPaths(agent, w, held(map[string]string{"spec": ""}))
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentPathUnavailable || rt.NodeID != "ask" {
			t.Fatalf("err = %v, want the typed refusal on node ask", err)
		}
	})
	t.Run("the execution boundary refuses before the executor runs", func(t *testing.T) {
		stub := newStubExecutor()
		called := false
		stub.on("scan", func(map[string]any) (map[string]any, error) { called = true; return map[string]any{}, nil })
		e := &Engine{executor: stub}
		ctx := model.WithTemplateData(context.Background(), held(map[string]string{"spec": "", "logo": "/x/logo.png"}))
		_, err := e.executeWithResources(ctx, tool, map[string]any{})
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentPathUnavailable {
			t.Fatalf("err = %v, want the typed refusal", err)
		}
		if called {
			t.Fatal("the executor ran a node whose attachment has no path")
		}
		ctx = model.WithTemplateData(context.Background(), held(map[string]string{"spec": "/s", "logo": "/l"}))
		if _, err := e.executeWithResources(ctx, tool, map[string]any{}); err != nil || !called {
			t.Fatalf("an admitted node did not run: err=%v called=%v", err, called)
		}
	})
}
