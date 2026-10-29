package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// threeActionNodes is the program the connector half of `validate` is asked
// about: three `action:` nodes whose fates differ only in what the installed
// catalogue holds — a connector no tier serves, an operation the served
// package does not declare, and one the package does declare. Node ids sorted
// (absent < mistyped < served) so the report's order is assertable.
const threeActionNodes = `## Fixture — three action nodes against a fixture connector catalogue.

dsl: 2

schema action_out:
  ok: bool

tool absent:
  action: nowhere.issue.get
  connection: main
  output: action_out

tool mistyped:
  action: probe.issue.nosuch
  connection: main
  output: action_out

tool served:
  action: probe.issue.get
  connection: main
  output: action_out

workflow three_actions:

  entry: served

  served -> mistyped

  mistyped -> absent

  absent -> done
`

// writeProbeCatalogPackage installs the `probe` connector package under root,
// which is a catalog ROOT (the package lives in a directory named after its
// own id — FSCatalog refuses the two disagreeing).
func writeProbeCatalogPackage(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pkg := &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			DisplayName: "Probe",
			BaseURL:     spec.BaseURL{Default: "https://probe.example", PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization",
			}},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{{
				ID: "probe.issue.get", Resource: "issue", Verb: "get",
				HTTP:   spec.HTTPBinding{Method: "GET", Path: "/issues/{id}"},
				Effect: spec.EffectRead, Deterministic: true,
				Params:  []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
				Results: []spec.ResultCase{{Status: 200}},
			}},
		}},
	}
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("write package: %v", err)
	}
}

// validateAsJSON runs `validate` with its options and hands back BOTH the
// bytes a caller reads and the decoded result: the bytes are the contract for
// the default invocation (below), the struct is what the assertions read.
func validateAsJSON(t *testing.T, path string, opts cli.ValidateOptions) ([]byte, cli.ValidateResult) {
	t.Helper()
	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunValidateWith(path, p, opts); err != nil {
		t.Fatalf("validate %s: %v\n%s", path, err, buf.String())
	}
	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", err, buf.String())
	}
	return buf.Bytes(), result
}

// aCatalogWithTwoTiers is the install the resolution cases run against: a
// granted project tier that holds nothing and a home tier that holds `probe`.
// Returns the two roots, in the order a lookup consults them.
func aCatalogWithTwoTiers(t *testing.T) (project, home string) {
	t.Helper()
	workspace, iterionHome := t.TempDir(), t.TempDir()
	// The project tier is the cwd's, so the command runs where the workspace
	// is — the same reading `connections add` does, and the one an operator
	// typing the command in their project expects.
	t.Chdir(workspace)
	t.Setenv("ITERION_HOME", iterionHome)
	t.Setenv(connection.ProjectCatalogEnv, "1")
	project = filepath.Join(workspace, "connectors")
	home = filepath.Join(iterionHome, "connectors")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir project tier: %v", err)
	}
	writeProbeCatalogPackage(t, home)
	return project, home
}

// TestValidateConnectorsResolvesEachActionAgainstTheInstalledCatalogue is the
// witness of what `--connectors` answers, node by node.
//
// A compile checks the SHAPE of an action id and says so in its own doc
// (pkg/dsl/ir/compile_action.go: "whether the operation exists is a question
// for the connector package, which `iterion validate` resolves separately").
// This is that resolution: the package tiers of the install, read exactly as a
// run reads them, and one finding per node they cannot serve — naming which
// tiers were consulted when nothing holds the connector, and naming the
// operation when the package that IS installed does not declare it.
func TestValidateConnectorsResolvesEachActionAgainstTheInstalledCatalogue(t *testing.T) {
	project, home := aCatalogWithTwoTiers(t)
	path := writeFixture(t, t.TempDir(), "three.bot", threeActionNodes)

	_, result := validateAsJSON(t, path, cli.ValidateOptions{Connectors: true})
	if !result.Valid {
		t.Fatalf("the fixture must compile: %+v", result.Diagnostics)
	}
	if result.Connectors == nil {
		t.Fatal("--connectors produced no report")
	}
	if result.Connectors.Nodes != 3 {
		t.Errorf("nodes = %d, want the program's 3 action nodes", result.Connectors.Nodes)
	}
	if result.Connectors.NoCatalog != "" {
		t.Errorf("no_catalog = %q, want empty: two tiers were built", result.Connectors.NoCatalog)
	}

	var ids []string
	byNode := map[string]cli.ConnectorFinding{}
	for _, f := range result.Connectors.Findings {
		ids = append(ids, f.NodeID)
		byNode[f.NodeID] = f
	}
	// "served" is NOT here, and that is the assertion: a report of what the
	// install cannot serve, not a line per node.
	if want := []string{"absent", "mistyped"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("findings = %v, want exactly %v in node-id order (a servable node stays silent)", ids, want)
	}

	// A connector NO tier serves: the reader is told which directories were
	// looked in, both of them — only the last one sends them to the wrong
	// place.
	noTier := byNode["absent"]
	if noTier.Action != "nowhere.issue.get" || noTier.Connector != "nowhere" || noTier.Connection != "main" {
		t.Errorf("finding = %+v, want it to carry the action, its connector and the binding alias", noTier)
	}
	for _, root := range []string{project, home} {
		if !strings.Contains(noTier.Reason, root) {
			t.Errorf("reason for an unserved connector = %q, want it to name the tier at %s", noTier.Reason, root)
		}
	}

	// A connector that IS installed, without the operation: the operation is
	// what has to be corrected, so the operation is what is named.
	noOp := byNode["mistyped"]
	if !strings.Contains(noOp.Reason, "probe.issue.nosuch") {
		t.Errorf("reason = %q, want it to name the operation the package does not declare", noOp.Reason)
	}
	if !strings.Contains(noOp.Reason, "v1.0.0") {
		t.Errorf("reason = %q, want it to name the version of the package that answered", noOp.Reason)
	}

	// The human output carries the same two findings and the same reasons —
	// an operator reading the terminal is the first reader of this report.
	p, buf := newTestPrinter(cli.OutputHuman)
	if err := cli.RunValidateWith(path, p, cli.ValidateOptions{Connectors: true}); err != nil {
		t.Fatalf("validate (human): %v", err)
	}
	out := buf.String()
	for _, want := range []string{"connectors: 2 of 3", "absent (nowhere.issue.get)", "mistyped (probe.issue.nosuch)"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "served (probe.issue.get)") {
		t.Errorf("human output warns on a node the catalogue serves:\n%s", out)
	}
}

// TestValidateConnectorsSaysOnceThatNothingIsInstalled pins the honesty of the
// empty case.
//
// An install with no connector catalogue at all is ONE fact about the machine,
// not three about the program: said once, naming the roots it would have read,
// with no finding on any node. A finding per node reads as N defects in the
// `.bot` and sends the author to correct action ids that are correct.
func TestValidateConnectorsSaysOnceThatNothingIsInstalled(t *testing.T) {
	iterionHome := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("ITERION_HOME", iterionHome)
	t.Setenv(connection.ProjectCatalogEnv, "")
	path := writeFixture(t, t.TempDir(), "three.bot", threeActionNodes)

	_, result := validateAsJSON(t, path, cli.ValidateOptions{Connectors: true})
	if result.Connectors == nil {
		t.Fatal("--connectors produced no report")
	}
	if len(result.Connectors.Findings) != 0 {
		t.Fatalf("findings = %+v, want none: with no catalogue every node has the same single cause, and it is the install's", result.Connectors.Findings)
	}
	if result.Connectors.Nodes != 3 {
		t.Errorf("nodes = %d, want 3: the nodes were counted even though none could be resolved", result.Connectors.Nodes)
	}
	if !strings.Contains(result.Connectors.NoCatalog, filepath.Join(iterionHome, "connectors")) {
		t.Errorf("no_catalog = %q, want it to name the home root it would have read", result.Connectors.NoCatalog)
	}
	if !strings.Contains(result.Connectors.NoCatalog, connection.ProjectCatalogEnv) {
		t.Errorf("no_catalog = %q, want the project tier named as the grant it is", result.Connectors.NoCatalog)
	}
	// The same sentence `connections add` prints, from the same function: an
	// operator must not read two wordings of one install fact.
	p, buf := newTestPrinter(cli.OutputHuman)
	if err := cli.RunValidateWith(path, p, cli.ValidateOptions{Connectors: true}); err != nil {
		t.Fatalf("validate (human): %v", err)
	}
	if !strings.Contains(buf.String(), "connectors: no connector catalog found") {
		t.Errorf("human output does not say the install holds no catalogue:\n%s", buf.String())
	}
}

// TestValidateTheDefaultInvocationDoesNotReadTheMachine is the reason the flag
// is opt-in, as a test rather than as a sentence.
//
// What a tier serves is a property of the INSTALL: the same `.bot` has a
// servable action on the laptop that generated the package and an unservable
// one in CI. So `validate` without `--connectors` must answer the same bytes
// whatever is installed — otherwise a verdict read in CI would be about the
// runner's disk, and the corpus walk this package already runs over every
// `.bot` in the tree would depend on the developer's home.
func TestValidateTheDefaultInvocationDoesNotReadTheMachine(t *testing.T) {
	// ONE bot file and ONE cwd for both readings: only the installed
	// catalogue changes between them, which is the variable under test.
	path := writeFixture(t, t.TempDir(), "three.bot", threeActionNodes)
	workspace := t.TempDir()
	t.Chdir(workspace)

	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv(connection.ProjectCatalogEnv, "")
	bare, result := validateAsJSON(t, path, cli.ValidateOptions{})
	if result.Connectors != nil || result.ConnectorsError != "" {
		t.Fatalf("the default invocation resolved connectors: %+v / %q", result.Connectors, result.ConnectorsError)
	}
	if strings.Contains(string(bare), "connectors") {
		t.Fatalf("the default JSON carries a connector key:\n%s", bare)
	}

	iterionHome := t.TempDir()
	t.Setenv("ITERION_HOME", iterionHome)
	t.Setenv(connection.ProjectCatalogEnv, "1")
	if err := os.MkdirAll(filepath.Join(workspace, "connectors"), 0o755); err != nil {
		t.Fatalf("mkdir project tier: %v", err)
	}
	writeProbeCatalogPackage(t, filepath.Join(iterionHome, "connectors"))
	installed, _ := validateAsJSON(t, path, cli.ValidateOptions{})

	if string(bare) != string(installed) {
		t.Fatalf("the default invocation changed with the install:\n--- no catalogue ---\n%s\n--- a catalogue serving one of the three actions ---\n%s", bare, installed)
	}
	// The falsifier of the comparison itself: ASKED, this install answers
	// something — so the bytes above are equal because the default reads the
	// program alone, not because the fixture's actions are indistinguishable
	// or the catalogue was never built.
	_, asked := validateAsJSON(t, path, cli.ValidateOptions{Connectors: true})
	if asked.Connectors == nil || len(asked.Connectors.Findings) != 2 {
		t.Fatalf("asked, this install must serve exactly one of the three actions: %+v", asked.Connectors)
	}
}

// TestValidateConnectorsSaysACatalogRootItCannotRead keeps the one failure
// that is not an answer apart from the ones that are.
//
// A root that is ABSENT is "nothing installed", and the report says so. A root
// that is THERE and cannot be read is a question the operator asked and did
// not get answered: it is named, and the command exits non-zero — while the
// compile verdict, which owes nothing to the catalogue, still stands and is
// printed.
func TestValidateConnectorsSaysACatalogRootItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	parent := t.TempDir()
	iterionHome := filepath.Join(parent, "home")
	if err := os.MkdirAll(filepath.Join(iterionHome, "connectors"), 0o755); err != nil {
		t.Fatalf("mkdir home tier: %v", err)
	}
	// Unreadable THROUGH ITS PARENT: the directory is there, and the stat
	// fails with something that is not ErrNotExist.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	t.Chdir(t.TempDir())
	t.Setenv("ITERION_HOME", iterionHome)
	t.Setenv(connection.ProjectCatalogEnv, "")
	path := writeFixture(t, t.TempDir(), "three.bot", threeActionNodes)

	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunValidateWith(path, p, cli.ValidateOptions{Connectors: true})
	if err == nil {
		t.Fatal("a catalog root that is there and cannot be read must fail the command, not read as an empty install")
	}
	var result cli.ValidateResult
	if jerr := json.Unmarshal(buf.Bytes(), &result); jerr != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", jerr, buf.String())
	}
	if result.Connectors != nil {
		t.Errorf("connectors = %+v, want no report: nothing was resolved", result.Connectors)
	}
	if !strings.Contains(result.ConnectorsError, filepath.Join(iterionHome, "connectors")) {
		t.Errorf("connectors_error = %q, want it to name the root that could not be read", result.ConnectorsError)
	}
	if !result.Valid {
		t.Errorf("the compile verdict must stand on its own: %+v", result.Diagnostics)
	}

	// And it is a finding about a program that ASKS the catalogue something: a
	// `.bot` with no `action:` node reads no root, so the same broken install
	// cannot fail a question that never touches it.
	noAction := writeFixture(t, t.TempDir(), "plain.bot", "tool probe:\n  command: \"echo hi\"\n\nworkflow plain:\n  entry: probe\n  probe -> done\n")
	// Read through the entry point rather than the helper: the point is that
	// this invocation does NOT fail, so the error is the assertion.
	pp, pbuf := newTestPrinter(cli.OutputJSON)
	if perr := cli.RunValidateWith(noAction, pp, cli.ValidateOptions{Connectors: true}); perr != nil {
		t.Fatalf("a program with no action node must not fail on a root it never reads: %v\n%s", perr, pbuf.String())
	}
	var plain cli.ValidateResult
	if jerr := json.Unmarshal(pbuf.Bytes(), &plain); jerr != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", jerr, pbuf.String())
	}
	if plain.ConnectorsError != "" {
		t.Errorf("connectors_error = %q on a program with no action node: the unreadable root was read for nothing", plain.ConnectorsError)
	}
	if plain.Connectors == nil || plain.Connectors.Nodes != 0 || plain.Connectors.NoCatalog != "" || len(plain.Connectors.Findings) != 0 {
		t.Errorf("connectors = %+v, want an empty report: asked, and there was nothing to resolve", plain.Connectors)
	}
}
