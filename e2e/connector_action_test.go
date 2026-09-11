package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The connector action, end to end: a `.bot` compiled from source, executed by
// the real engine, reaching a real HTTP server through a real connection.
//
// Every layer below has its own tests, and all of them passed while the path
// as a whole did not exist. This is the only test that fails if a `.bot`
// cannot actually call a vendor — which is the promise, and the thing none of
// the unit tests can observe.

const connectorBot = `
vars:
  issue: string = "42"

tool comment:
  action: probe.issue.comment
  connection: main
  params:
    owner: "acme"
    repo: "widgets"
    index: "{{vars.issue}}"
    body: "shipped"
  timeout: 30s
  output: comment_result

schema comment_result:
  status: int

workflow main:
  entry: comment
  comment -> done
`

// probePkg writes a minimal connector package into a catalog root, the way a
// generated one would sit on disk.
func writeProbePackage(t *testing.T, root, baseURL string) {
	t.Helper()
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pkg := &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			DisplayName: "Probe",
			BaseURL:     spec.BaseURL{Default: baseURL, PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header",
				Name: "Authorization", ValuePrefix: "token ",
			}},
			Maturity: spec.MaturityQualified,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{{
				ID: "probe.issue.comment", Resource: "issue", Verb: "comment",
				HTTP: spec.HTTPBinding{
					Method: "POST", Path: "/repos/{owner}/{repo}/issues/{index}/comments",
					RequestBody: spec.BodyJSON,
				},
				Effect: spec.EffectCreate, Deterministic: true,
				Params: []spec.Param{
					{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
					{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
					{Key: "index", Name: "index", In: spec.InPath, Type: "integer", Required: true},
					{Key: "body", Name: "body", In: spec.InBody, Type: "string", Required: true},
				},
				Results: []spec.ResultCase{{Status: 201}},
			}},
		}},
	}
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("write package: %v", err)
	}
}

// TestAConnectorActionReachesTheVendor runs the whole path.
func TestAConnectorActionReachesTheVendor(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id": 7}`))
	}))
	defer srv.Close()

	resolver := seedProbeConnection(t, t.TempDir(), srv.URL)

	out, _, err := runConnectorBot(t, connectorBot, resolver, srv.Client())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The REQUEST is the oracle: what the vendor received settles whether the
	// path works, not what the node returned.
	if gotPath != "/api/v1/repos/acme/widgets/issues/42/comments" {
		t.Errorf("path = %q — the package's prefix, the operation's template and the run's var must all have applied", gotPath)
	}
	if gotAuth != "token s3cret" {
		t.Errorf("Authorization = %q, want the sealed credential behind the package's prefix", gotAuth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body is not JSON: %q", gotBody)
	}
	if body["body"] != "shipped" {
		t.Errorf("body = %v, want the declared body member", body)
	}
	// `index` is declared an integer, and the template rendered a string:
	// the coercion has to have happened, or a vendor with a strict schema
	// refuses the call.
	if strings.Contains(gotPath, `"42"`) {
		t.Errorf("path carries a quoted number: %q", gotPath)
	}

	if got := fmt.Sprint(out["status"]); got != "201" {
		t.Errorf("node output status = %v, want 201", out["status"])
	}
}

// TestAnActionWithNoCatalogFailsEXPLICITLY. A bot that declares a connector
// call and runs somewhere with no catalog must say so — reporting success for
// a call it never made is the one outcome that must be impossible.
func TestAnActionWithNoCatalogFailsExplicitly(t *testing.T) {
	_, _, err := runConnectorBot(t, connectorBot, nil, nil)
	if err == nil {
		t.Fatal("a connector action with no catalog wired must fail the run")
	}
	if !strings.Contains(err.Error(), "connector catalog") {
		t.Errorf("error = %v, want it to name the missing catalog", err)
	}
}

// seedProbeConnection writes the probe package into a catalog under dir and
// creates an active connection holding a real sealed credential, pinned to
// baseURL. The resolver it returns is what the executor seam consumes.
func seedProbeConnection(t *testing.T, dir, baseURL string) *connection.Resolver {
	t.Helper()
	catalogRoot := filepath.Join(dir, "connectors")
	writeProbePackage(t, catalogRoot, baseURL)

	// A real sealed credential in a real file store.
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st, err := connection.NewFileStore(connection.DefaultPath(dir))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sealed, err := connection.SealToken(sealer, "conn1", "s3cret", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := st.Create(context.Background(), connection.Connection{
		ID: "conn1", TenantID: connection.LocalTenant, Connector: "probe", Alias: "main",
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: []connection.Capability{connection.CapAction},
		// The origin is PINNED on the record, as `connections add` pins it:
		// nothing may later redirect this credential to another host.
		BaseURL:       baseURL,
		SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	return &connection.Resolver{
		Catalog:  connection.NewFSCatalog(catalogRoot),
		Store:    st,
		Sealer:   sealer,
		TenantID: connection.LocalTenant,
	}
}

// TestAnUndecidedMutationParksTheRunForAnOperator proves the MIDDLE of the
// ambiguity chain — the half neither end's own test can see.
//
// One end is the connector deciding a mutation's effect is undecided; the
// other is retrypolicy refusing to auto-resume AMBIGUOUS_EFFECT. Both have
// tests. What sits between them is the ENGINE persisting that verdict on the
// run, and it is not free: the sibling helper failRunWithCheckpoint writes
// EXECUTION_FAILED unconditionally, and EXECUTION_FAILED is on the auto-resume
// allow-list. Routed there, an undecided mutation becomes an ordinary retry —
// the duplicate this whole class exists to prevent, reintroduced by a refactor
// that no other test would redden.
func TestAnUndecidedMutationParksTheRunForAnOperator(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		// The answer is lost AFTER the request left: the write may or may not
		// have landed, and nothing iterion holds can say which.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	resolver := seedProbeConnection(t, t.TempDir(), srv.URL)

	// The engine's REAL recovery dispatch, wired as every production host
	// wires it (pkg/cli/run.go, pkg/runview, the cloud runner, the
	// dispatcher). Without it nothing classifies, and the test would certify
	// a wiring nobody runs.
	_, r, err := runConnectorBot(t, connectorBot, resolver, srv.Client(),
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())))
	if err == nil {
		t.Fatal("a 500 on a mutation must fail the run")
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("the vendor saw %d requests — an undecided mutation must be sent exactly once", got)
	}
	if r.FailureCode != store.FailureAmbiguousEffect {
		t.Errorf("failure code = %q, want %q — the run carries the classification an operator greps and every automatic resume keys on",
			r.FailureCode, store.FailureAmbiguousEffect)
	}
	// Resumable, and that is the point: reconciling the remote state is a
	// HUMAN act, and the checkpoint is what lets a deliberate `iterion resume`
	// follow it.
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %q, want %q", r.Status, store.RunStatusFailedResumable)
	}
	// Asked through the production predicate rather than a literal: if the
	// disposition table is ever edited, this reddens with it.
	if retrypolicy.AutoResumable(r.FailureCode) {
		t.Error("AMBIGUOUS_EFFECT became auto-resumable — a scheduler would re-drive a mutation that may already have landed")
	}
}

// runConnectorBot compiles the source, builds the REAL executor with the
// connector seam wired, and runs it on the real engine. Returns the tool
// node's output.
func runConnectorBot(t *testing.T, src string, resolver *connection.Resolver, client *http.Client, opts ...runtime.EngineOption) (map[string]any, *store.Run, error) {
	t.Helper()
	wf := compileSource(t, "connector.bot", src)

	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := "e2e-connector-" + t.Name()
	if _, err := s.CreateRun(context.Background(), runID, wf.Name, nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	espec := runview.ExecutorSpec{
		Workflow: wf,
		Store:    s,
		RunID:    runID,
		StoreDir: storeDir,
	}
	// Nil stays nil: the no-catalog case must go through the same builder, or
	// it would be testing a different wiring than the one that ships.
	if resolver != nil {
		espec.Connectors = resolver
		espec.ConnectorClient = client
	}
	exec, err := runview.BuildExecutor(espec)
	if err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}

	eng := newEngine(t, wf, s, exec, opts...)
	runErr := eng.Run(context.Background(), runID, nil)
	// The PERSISTED run travels back on BOTH paths. What a failure leaves on
	// the record — its status and its classification — is the thing a caller
	// may be here to assert, and returning only the error would hide it.
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if runErr != nil {
		return nil, r, runErr
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("run status = %s, want finished", r.Status)
	}
	if art, aerr := s.LoadLatestArtifact(context.Background(), runID, "comment"); aerr == nil {
		return art.Data, r, nil
	}
	// No artifact: the node's output still reached the checkpoint, which is
	// the authority the engine resumes from.
	if r.Checkpoint != nil {
		if out, ok := r.Checkpoint.Outputs["comment"]; ok {
			return out, r, nil
		}
	}
	t.Fatalf("the node produced no readable output")
	return nil, nil, nil
}
