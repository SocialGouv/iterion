package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A subbot node must project onto the wire as kind "subbot" carrying the
// child .bot source path and the isolated flag (contract C2) — the studio
// canvas uses them to render the child-workflow affordance.
const subbotExportSrc = `
schema child_out:
  verdict: string

tool prep:
  description: "Prepare the episode payload"
  command: ` + "`printf '{\"ready\":true}'`" + `
  output: child_out

subbot produce_episode:
  source: "episode.bot"
  with {
    episode: "{{outputs.prep.verdict}}",
  }
  output: child_out
  isolated: true

workflow parent:
  entry: prep
  prep -> produce_episode
  produce_episode -> done
`

// A human gate's declared `input:` schema must reach the wire alongside
// its `output:` schema. The studio types the gate's INBOUND payload
// (Interaction.Questions) with it — a `json` field renders as structured
// data, a `file` field as a preview — so the operator sees what they are
// validating instead of the author stringifying it into `instructions:`.
const humanGateSchemaExportSrc = `
schema draft_out:
  plan: json
  summary: string

schema gate_in:
  plan: json
  summary: string
  mockup: file

schema gate_out:
  approved: bool
  notes: string

prompt draft_system:
  Draft the plan.

agent draft:
  system: draft_system
  input: gate_in
  output: draft_out

human gate:
  input: gate_in
  output: gate_out
  interaction: human

workflow gated:
  entry: draft
  draft -> gate with {
    plan: "{{outputs.draft.plan}}",
    summary: "{{outputs.draft.summary}}",
  }
  gate -> done when approved
  gate -> fail when not approved
`

func TestBuildWireWorkflow_HumanNodeInputSchemaProjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gated.bot")
	if err := os.WriteFile(path, []byte(humanGateSchemaExportSrc), 0o644); err != nil {
		t.Fatalf("write gated bot: %v", err)
	}

	wire, err := buildWireWorkflowFromRun(&store.Run{ID: "r1", FilePath: path}, nil)
	if err != nil {
		t.Fatalf("buildWireWorkflowFromRun: %v", err)
	}

	var gate, draft *WireNode
	for i := range wire.Nodes {
		switch wire.Nodes[i].ID {
		case "gate":
			gate = &wire.Nodes[i]
		case "draft":
			draft = &wire.Nodes[i]
		}
	}
	if gate == nil {
		t.Fatalf("human node missing from projection: %+v", wire.Nodes)
	}

	wantIn := map[string]string{"plan": "json", "summary": "string", "mockup": "file"}
	if len(gate.InputFields) != len(wantIn) {
		t.Fatalf("input_schema = %+v, want %d fields", gate.InputFields, len(wantIn))
	}
	for _, f := range gate.InputFields {
		want, ok := wantIn[f.Name]
		if !ok {
			t.Errorf("unexpected input field %q", f.Name)
			continue
		}
		if f.Type != want {
			t.Errorf("input field %q type = %q, want %q", f.Name, f.Type, want)
		}
	}

	// The output schema still projects — the answer form depends on it.
	if len(gate.OutputFields) != 2 {
		t.Errorf("output_schema = %+v, want 2 fields", gate.OutputFields)
	}

	// input_schema stays a human-node projection: an agent node declaring
	// the very same schema as its INPUT must not gain the field (nothing
	// renders a payload for a node that never pauses, and the wire stays
	// as small as the canvas needs it).
	if draft == nil {
		t.Fatalf("agent node missing from projection: %+v", wire.Nodes)
	}
	if len(draft.InputFields) != 0 {
		t.Errorf("agent node carries input_schema: %+v", draft.InputFields)
	}
}

func TestBuildWireWorkflow_SubbotNodeProjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.bot")
	if err := os.WriteFile(path, []byte(subbotExportSrc), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	wire, err := buildWireWorkflowFromRun(&store.Run{ID: "r1", FilePath: path}, nil)
	if err != nil {
		t.Fatalf("buildWireWorkflowFromRun: %v", err)
	}

	var subbot *WireNode
	for i := range wire.Nodes {
		if wire.Nodes[i].ID == "produce_episode" {
			subbot = &wire.Nodes[i]
		}
	}
	if subbot == nil {
		t.Fatalf("subbot node missing from projection: %+v", wire.Nodes)
	}
	if subbot.Kind != "subbot" {
		t.Errorf("kind = %q, want %q", subbot.Kind, "subbot")
	}
	if subbot.Source != "episode.bot" {
		t.Errorf("source = %q, want %q", subbot.Source, "episode.bot")
	}
	if !subbot.Isolated {
		t.Error("isolated = false, want true")
	}

	// Non-subbot nodes must not leak the fields (omitempty on the wire).
	for _, n := range wire.Nodes {
		if n.ID == "prep" && (n.Source != "" || n.Isolated) {
			t.Errorf("tool node carries subbot fields: %+v", n)
		}
	}

	// The authored `description:` projects onto the wire node; nodes
	// without one stay empty (omitempty on the wire).
	for _, n := range wire.Nodes {
		switch n.ID {
		case "prep":
			if n.Description != "Prepare the episode payload" {
				t.Errorf("prep description = %q, want %q", n.Description, "Prepare the episode payload")
			}
		case "produce_episode":
			if n.Description != "" {
				t.Errorf("produce_episode description = %q, want empty", n.Description)
			}
		}
	}
}
