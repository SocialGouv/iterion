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
