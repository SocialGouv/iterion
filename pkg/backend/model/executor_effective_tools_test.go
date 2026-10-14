package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// imageWorkflow is a program whose only opt-in is an image attachment a prompt
// references — the append that is decided by the PROGRAM and by nothing on the
// host.
func imageWorkflow(backend string) (*ir.Workflow, ir.Node) {
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: backend, UserPrompt: "p"},
		Tools:     []string{"read_file"},
	}
	wf := &ir.Workflow{
		Nodes:       map[string]ir.Node{"a": node},
		Attachments: map[string]*ir.Attachment{"shot": {Name: "shot", Type: ir.AttachmentImage}},
		Prompts:     map[string]*ir.Prompt{"p": {Name: "p", Body: "look at {{attachments.shot}}"}},
	}
	return wf, node
}

// TestTheProgramOnlyReadingAnswersTheProgramsOwnAppends keeps the two readings
// of the effective surface on the same answer wherever the PROGRAM decides it.
//
// NewProgramExecutor exists so a static check does not depend on the machine it
// runs on, and `iterion validate --exec --strict` reads it while the engine
// reads the host's — so every field the program itself carries must reach both,
// or the two disagree about one file. The image-attachment append is such a
// field: on every backend but claw it puts `read_image` on the node's list, and
// codex sandboxes that name workspace-write, so the reading that missed it
// admitted a parallel fan-out the engine refuses.
//
// `vars:` is the stated exception, and the last row says so: a caller owes
// SetVars before asking either seam, and until it does the program-only
// reading is the pessimistic one, which is the side a pre-run guard takes.
func TestTheProgramOnlyReadingAnswersTheProgramsOwnAppends(t *testing.T) {
	// The class this gate reads is the fields the PROGRAM carries. The host's
	// reading also resolves `auto_memory:` from the machine, which the
	// program-only one deliberately does not, so a host that exports the name
	// makes the two differ by design — and this gate would report it as a
	// program field that failed to reach both readings, sending its reader at
	// the wrong half.
	t.Setenv(automemory.ModeEnv, "")
	for _, backend := range []string{delegate.BackendCodex, delegate.BackendClaudeCode, delegate.BackendPi} {
		t.Run(backend, func(t *testing.T) {
			wf, node := imageWorkflow(backend)
			host := NewClawExecutor(NewRegistry(), wf).EffectiveToolNames(node, false)
			program := NewProgramExecutor(wf).EffectiveToolNames(node, false)
			if !slices.Contains(host, "read_image") {
				t.Fatalf("the host's reading holds %v, without `read_image`: the append this test is about is gone, so it tests nothing", host)
			}
			if !slices.Equal(host, program) {
				t.Errorf("the host's reading holds %v and the program-only one %v: a field the program itself carries must reach both, or `iterion validate --exec` and `iterion run` disagree about one file",
					host, program)
			}
		})
	}
	t.Run("a `vars:` default is the caller's to supply, and the program reads less until it does", func(t *testing.T) {
		varsWorkflow := func() (*ir.Workflow, ir.Node) {
			node := &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "a"},
				LLMFields: ir.LLMFields{Backend: "{{vars.b}}"},
				Tools:     []string{"read_file"},
			}
			return &ir.Workflow{
				Nodes: map[string]ir.Node{"a": node},
				Vars:  map[string]*ir.Var{"b": {Name: "b", Type: ir.VarString, Default: delegate.BackendClaw, HasDefault: true}},
			}, node
		}
		wf, node := varsWorkflow()
		host := NewClawExecutor(NewRegistry(), wf).EffectiveToolNames(node, false)
		program := NewProgramExecutor(wf).EffectiveToolNames(node, false)
		if slices.Equal(host, program) {
			t.Fatalf("both readings hold %v: this row is about the one program-carried input the program-only executor leaves to its caller, so it tests nothing", host)
		}
		// The difference may only go one way: what the program reads is a
		// SUBSET of what the host does, so the route stays unresolved there
		// and admission refuses rather than admits.
		for _, name := range program {
			if !slices.Contains(host, name) {
				t.Errorf("the program-only reading holds %q and the host's does not (%v vs %v): an unseeded `vars:` route must read as unresolved, which is the pessimistic side, never as a route that bounds the node",
					name, program, host)
			}
		}
		seeded := NewProgramExecutor(wf)
		seeded.SetVars(map[string]any{"b": delegate.BackendClaw})
		if got := seeded.EffectiveToolNames(node, false); !slices.Equal(host, got) {
			t.Errorf("after SetVars the program-only reading holds %v and the host's %v: the caller supplied the one input the constructor leaves open, so the two must agree from there on",
				got, host)
		}
	})
	t.Run("claw receives no image append and the two still agree", func(t *testing.T) {
		wf, node := imageWorkflow(delegate.BackendClaw)
		host := NewClawExecutor(NewRegistry(), wf).EffectiveToolNames(node, false)
		program := NewProgramExecutor(wf).EffectiveToolNames(node, false)
		if slices.Contains(host, "read_image") {
			t.Errorf("claw holds %v: the append is for the backends that cannot take inline images, and granting it here would widen claw's surface for nothing", host)
		}
		if !slices.Equal(host, program) {
			t.Errorf("the host's reading holds %v and the program-only one %v", host, program)
		}
	})
}
