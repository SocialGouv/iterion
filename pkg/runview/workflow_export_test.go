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
}

func TestBuildWireWorkflow_ResolvesRelativeFilePathFromRunWorkDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.bot")
	if err := os.WriteFile(path, []byte(subbotExportSrc), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	wire, err := buildWireWorkflowFromRun(&store.Run{
		ID:       "relative-source",
		FilePath: "parent.bot",
		WorkDir:  dir,
	}, nil)
	if err != nil {
		t.Fatalf("buildWireWorkflowFromRun: %v", err)
	}
	if wire.Name != "parent" {
		t.Fatalf("workflow name = %q, want parent", wire.Name)
	}
}
