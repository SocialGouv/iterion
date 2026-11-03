package ir

import "testing"

// images is the per-node list of input image paths (templated) that the codex
// backend forwards as `-i` for image-to-image. It must survive parse → compile
// as a first-class AgentNode field, verbatim (templates resolved later at run).
const imagesSrc = `
schema in_s:
  x: string

schema out_s:
  y: bool

prompt sys:
  System.

prompt usr:
  User.

agent kf:
  backend: "codex"
  model: "gpt"
  input: in_s
  output: out_s
  system: sys
  user: usr
  images: ["{{input.x}}", "seed.png"]

workflow wf:
  entry: kf
  kf -> done
`

func TestCompileAgentImages(t *testing.T) {
	w := mustCompile(t, imagesSrc)

	kf, ok := w.Nodes["kf"].(*AgentNode)
	if !ok {
		t.Fatalf("kf is not an AgentNode")
	}
	if len(kf.Images) != 2 {
		t.Fatalf("kf.Images len = %d, want 2 (%v)", len(kf.Images), kf.Images)
	}
	if kf.Images[0] != "{{input.x}}" || kf.Images[1] != "seed.png" {
		t.Errorf("kf.Images = %v, want [{{input.x}} seed.png] (templates kept verbatim)", kf.Images)
	}
}

// A group's `{{params.X}}` is substituted before the node compile passes run,
// so the images walk and the template check it feeds never meet the params
// namespace — which ParseRefs does not know. The bound path reaches the
// instance verbatim.
const groupImagesSrc = `
schema out_s:
  y: bool

prompt usr:
  User.

group g(shot):
  agent work:
    model: "test-model"
    user: usr
    output: out_s
    images: ["{{params.shot}}.png"]
use g as r1 with {shot: "hero"}

workflow wf:
  entry: r1.work
  r1.work -> done
`

func TestAGroupsImagePathIsBoundBeforeTheWalkReadsIt(t *testing.T) {
	got := compileText(t, groupImagesSrc)
	for _, d := range got.Diagnostics {
		if d.Code == DiagBadTemplateRef {
			t.Fatalf("C004 fires on a group's bound image path: %s", d.Error())
		}
	}
	work, ok := got.Workflow.Nodes["r1.work"].(*AgentNode)
	if !ok {
		t.Fatalf("r1.work is not an AgentNode (nodes: %v)", got.Workflow.Nodes)
	}
	if len(work.Images) != 1 || work.Images[0] != "hero.png" {
		t.Errorf("r1.work images = %v, want [hero.png]", work.Images)
	}
}
