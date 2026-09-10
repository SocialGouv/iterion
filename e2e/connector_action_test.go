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
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
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

	dir := t.TempDir()
	catalogRoot := filepath.Join(dir, "connectors")
	writeProbePackage(t, catalogRoot, srv.URL)

	// A real sealed credential in a real file store.
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	storePath := connection.DefaultPath(dir)
	st, err := connection.NewFileStore(storePath)
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
		Capabilities:  []connection.Capability{connection.CapAction},
		SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	resolver := &connection.Resolver{
		Catalog:  connection.NewFSCatalog(catalogRoot),
		Store:    st,
		Sealer:   sealer,
		TenantID: connection.LocalTenant,
	}

	out, err := runConnectorBot(t, connectorBot, resolver, srv.Client())
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
	_, err := runConnectorBot(t, connectorBot, nil, nil)
	if err == nil {
		t.Fatal("a connector action with no catalog wired must fail the run")
	}
	if !strings.Contains(err.Error(), "connector catalog") {
		t.Errorf("error = %v, want it to name the missing catalog", err)
	}
}

// runConnectorBot compiles the source, builds the REAL executor with the
// connector seam wired, and runs it on the real engine. Returns the tool
// node's output.
func runConnectorBot(t *testing.T, src string, resolver *connection.Resolver, client *http.Client) (map[string]any, error) {
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

	eng := newEngine(t, wf, s, exec)
	runErr := eng.Run(context.Background(), runID, nil)
	if runErr != nil {
		return nil, runErr
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("run status = %s, want finished", r.Status)
	}
	if art, aerr := s.LoadLatestArtifact(context.Background(), runID, "comment"); aerr == nil {
		return art.Data, nil
	}
	// No artifact: the node's output still reached the checkpoint, which is
	// the authority the engine resumes from.
	if r.Checkpoint != nil {
		if out, ok := r.Checkpoint.Outputs["comment"]; ok {
			return out, nil
		}
	}
	t.Fatalf("the node produced no readable output")
	return nil, nil
}
