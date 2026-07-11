package ir

import "testing"

// full_access is the per-node opt-in that lifts the codex backend sandbox to
// danger-full-access (network egress). It must survive parse → compile as a
// first-class AgentNode field, and default to false when omitted.
const fullAccessSrc = `
schema in_s:
  x: string

schema out_s:
  y: bool

prompt sys:
  System.

prompt usr:
  User.

agent net:
  backend: "codex"
  model: "gpt"
  input: in_s
  output: out_s
  system: sys
  user: usr
  full_access: true

agent locked:
  backend: "codex"
  model: "gpt"
  input: in_s
  output: out_s
  system: sys
  user: usr

workflow wf:
  entry: net
  net -> locked
  locked -> done
`

func TestCompileAgentFullAccess(t *testing.T) {
	w := mustCompile(t, fullAccessSrc)

	net, ok := w.Nodes["net"].(*AgentNode)
	if !ok {
		t.Fatalf("net is not an AgentNode")
	}
	if !net.FullAccess {
		t.Errorf("net.FullAccess = false, want true (full_access: true opt-in lost)")
	}

	locked, ok := w.Nodes["locked"].(*AgentNode)
	if !ok {
		t.Fatalf("locked is not an AgentNode")
	}
	if locked.FullAccess {
		t.Errorf("locked.FullAccess = true, want false (default must stay locked)")
	}
}
