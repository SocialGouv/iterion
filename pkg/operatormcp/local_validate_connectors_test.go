package operatormcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connection"
)

// oneActionNode is a program whose single tool node declares a connector
// action — the one shape the connector resolution has anything to say about.
const oneActionNode = `dsl: 2

schema action_out:
  ok: bool

tool call:
  action: nowhere.issue.get
  connection: main
  output: action_out

workflow main:

  entry: call

  call -> done
`

// TestLocalValidateResolvesConnectorsOnlyWhenAsked keeps the agent's surface
// and the operator's at parity.
//
// `local_validate` is `iterion validate` for a validate loop an agent runs
// (docs/cli-reference.md says so), and every option of the command is an
// argument of the tool. The new one is no exception — an agent told "this
// action node is not servable here, install the package" fixes a `.bot` an
// agent told nothing about it would ship — and it carries the command's
// opt-in rule with it: unasked, the tool's answer is the same wherever it
// runs, because it never reads the machine's catalogue.
func TestLocalValidateResolvesConnectorsOnlyWhenAsked(t *testing.T) {
	s := newTestServer(t)
	if err := os.WriteFile(filepath.Join(s.WorkDir, "action.bot"), []byte(oneActionNode), 0o644); err != nil {
		t.Fatal(err)
	}
	// An iterion home of the test's own, with no catalogue in it: what the
	// tool answers must not depend on the operator's install.
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv(connection.ProjectCatalogEnv, "")

	text, isErr := call(t, s, "local_validate", `{"file_path":"action.bot","connectors":true}`)
	if isErr {
		t.Fatalf("asked for the connector resolution and got a tool error: %s", text)
	}
	var asked cli.ValidateResult
	if err := json.Unmarshal([]byte(text), &asked); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if !asked.Valid {
		t.Fatalf("the fixture must compile: %+v", asked.Diagnostics)
	}
	if asked.Connectors == nil {
		t.Fatalf("the tool took `connectors` and answered no report:\n%s", text)
	}
	if asked.Connectors.Nodes != 1 {
		t.Errorf("nodes = %d, want the program's one action node", asked.Connectors.Nodes)
	}
	if asked.Connectors.NoCatalog == "" {
		t.Errorf("connectors = %+v, want the empty install said once", asked.Connectors)
	}

	bare, isErr := call(t, s, "local_validate", `{"file_path":"action.bot"}`)
	if isErr {
		t.Fatalf("default invocation flagged as a tool error: %s", bare)
	}
	if strings.Contains(bare, "connectors") {
		t.Errorf("the default tool answer reads the machine's catalogue:\n%s", bare)
	}
}
