package runview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	connspec "github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// labelResolver serves one operation, `probe.issue.label`, whose `labels`
// body parameter is declared an array of strings — the shape #2991 met.
type labelResolver struct {
	pkg     *connspec.Package
	op      connspec.Operation
	baseURL string
}

func (r labelResolver) ResolveAction(context.Context, string, string) (*connspec.Package, connspec.Operation, exec.Credential, string, error) {
	return r.pkg, r.op, exec.Credential{SchemeID: "token", Value: "t"}, r.baseURL, nil
}

func newLabelResolver(base string) labelResolver {
	op := connspec.Operation{
		ID: "probe.issue.label", Resource: "issue", Verb: "label",
		HTTP:          connspec.HTTPBinding{Method: "POST", Path: "/labels", RequestBody: connspec.BodyJSON},
		Effect:        connspec.EffectRead,
		Deterministic: true,
		Params:        []connspec.Param{{Key: "labels", Name: "labels", In: connspec.InBody, Type: "array", Items: "string", Required: true}},
		Results:       []connspec.ResultCase{{Status: 200}},
	}
	pkg := &connspec.Package{
		Connector: connspec.Connector{
			SchemaVersion: connspec.SchemaVersion, ID: "probe", Version: "1.0.0",
			BaseURL:  connspec.BaseURL{Default: base},
			Auth:     []connspec.AuthScheme{{ID: "token", Kind: connspec.AuthAPIKey, In: "header", Name: "Authorization"}},
			Maturity: connspec.MaturityExperimental,
		},
		Ops: []connspec.OpsFile{{SchemaVersion: connspec.SchemaVersion, Connector: "probe", Domain: "issue", Operations: []connspec.Operation{op}}},
	}
	return labelResolver{pkg: pkg, op: op, baseURL: base}
}

const launchVarsSeedSource = `dsl: 2

vars:
  labels: string[]
  cfg: json

schema tagged:
  ok: bool

tool tag:
  action: probe.issue.label
  connection: main
  params:
    labels: "{{vars.labels}}"
  output: tagged

workflow seed:
  entry: tag
  tag -> done
`

// The executor BuildExecutor returns is seeded with the launch's values READ
// at each var's declared type, not with their text: a whole-value
// `{{vars.labels}}` on a `string[]` var reaches an `array` action parameter as
// the list the run reads (#2991), and a dotted `{{vars.cfg.backend}}` into a
// `json` var finds its field. This is the seed the engine's pre-run
// admissions resolve routing fields against, and the only one an engine-less
// caller of the executor ever sees.
//
// Mutation seen red (restored): BuildExecutor's seed put back to
// executor.SetVars(ir.DeclaredVarsOnly(spec.Workflow, spec.Vars)) — the node
// was refused (`param "labels": "a11y,rgaa" does not read as an array`) and
// the dotted field resolved to nothing.
func TestBuildExecutorSeedsLaunchVarsAtTheirDeclaredType(t *testing.T) {
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		mu.Lock()
		received = append(received, string(payload["labels"]))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cr := ir.Compile(parsertest.Parse("seed.bot", launchVarsSeedSource).File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %v", d)
		}
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor, err := BuildExecutor(ExecutorSpec{
		Ctx:             context.Background(),
		Store:           st,
		RunID:           "run-launch-vars-seed",
		Workflow:        cr.Workflow,
		WorkDir:         t.TempDir(),
		Vars:            map[string]string{"labels": "a11y,rgaa", "cfg": `{"backend":"claw"}`},
		Connectors:      newLabelResolver(srv.URL),
		ConnectorClient: exec.MarkGuarded(srv.Client()),
	})
	if err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}

	if _, err := executor.Execute(context.Background(), cr.Workflow.Nodes["tag"], nil); err != nil {
		t.Fatalf("a string[] launch value must reach an array action param as a list: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if len(got) != 1 || got[0] != `["a11y","rgaa"]` {
		t.Errorf("the vendor received %q, want one call carrying [\"a11y\",\"rgaa\"]", got)
	}
	if backend := executor.ResolveRoutingField("{{vars.cfg.backend}}"); backend != "claw" {
		t.Errorf("{{vars.cfg.backend}} resolved to %q, want %q — the json var was seeded as text", backend, "claw")
	}
}
