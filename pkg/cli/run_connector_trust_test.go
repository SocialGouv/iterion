package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A CLI run's connector resolution reads the RUN's trust, on the two builders
// every CLI run goes through: buildRunExecutor (a root `iterion run` under a
// carried ITERION_RUN_TRUST, a sub-bot child of an untrusted parent) and
// buildResumeExecutor (`iterion resume` of a fork run). Pinned "trusted" in
// either, a node process of a fork run that re-enters the CLI would resolve an
// `action:` node — mutating since #1802, a READ since ADR-162 decision 6
// withdrew those — and spend the operator's local connection credential,
// which the in-process path refuses.
//
// The fixture is the real builders over the operator's own tier, with no
// connection seeded, so nothing resolves either way: what tells the arms apart
// is WHICH refusal comes back. A trusted run gets as far as the tenant's
// connection store; an untrusted one is refused before it, typed; a builder
// nobody told states nothing and is refused too.

// connectorTrustFixture installs a package with one read and one mutating
// operation in the operator's tier of a scratch iterion home, pins a sealer
// that never reaches the OS keyring, and closes the workspace tier, so the
// operator's tier is the one that answers.
func connectorTrustFixture(t *testing.T) (store.RunStore, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	t.Setenv(connection.ProjectCatalogEnv, "")
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/iterion-test")
	t.Chdir(t.TempDir())
	op := func(id, verb, method string, effect spec.Effect, status int) spec.Operation {
		return spec.Operation{
			ID: id, Resource: "issue", Verb: verb,
			HTTP:   spec.HTTPBinding{Method: method, Path: "/issues/{id}"},
			Effect: effect, Deterministic: true,
			Params:  []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
			Results: []spec.ResultCase{{Status: status}},
		}
	}
	if err := spec.Write(filepath.Join(home, "connectors", "probe"), &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			DisplayName: "Probe", Maturity: spec.MaturityExperimental,
			BaseURL: spec.BaseURL{Default: "https://probe.example", PathPrefix: "/api/v1"},
			Auth:    []spec.AuthScheme{{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"}},
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{
				op("probe.issue.get", "get", "GET", spec.EffectRead, 200),
				op("probe.issue.comment", "comment", "POST", spec.EffectCreate, 201),
			},
		}},
	}); err != nil {
		t.Fatalf("write package: %v", err)
	}
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, storeDir
}

func mutatingActionNode() *ir.ToolNode {
	return &ir.ToolNode{BaseNode: ir.BaseNode{ID: "comment"}, Action: "probe.issue.comment", Connection: "main"}
}

func readActionNode() *ir.ToolNode {
	return &ir.ToolNode{BaseNode: ir.BaseNode{ID: "get"}, Action: "probe.issue.get", Connection: "main"}
}

// actionWorkflowFor builds the one-node workflow an arm executes: the node
// the arm names is the workflow's whole graph.
func actionWorkflowFor(node ir.Node) *ir.Workflow {
	return &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{node.NodeID(): node}}
}

// connectorTrustArm is what one builder's executor answers for the action: the
// refusal it is expected to come back with.
type connectorTrustArm struct {
	name string
	// refusal is the typed trust refusal the resolution must return, nil for
	// a trusted run, which must reach the tenant's connection store instead.
	refusal error
}

func assertConnectorTrustArm(t *testing.T, arm connectorTrustArm, exec runtime.NodeExecutor, node ir.Node) {
	t.Helper()
	if c, ok := exec.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{"id": 1})
	if err == nil {
		t.Fatal("no connection is seeded, so the action must not resolve")
	}
	if arm.refusal == nil {
		if errors.Is(err, connection.ErrUntrustedAction) || errors.Is(err, connection.ErrTrustUnstated) {
			t.Fatalf("a trusted run's action was refused on trust: %v", err)
		}
		if !strings.Contains(err.Error(), "connection named") {
			t.Fatalf("error = %v, want the tenant's connection store to have been consulted", err)
		}
		return
	}
	if !errors.Is(err, arm.refusal) {
		t.Fatalf("error = %v, want %v: the resolver was built for a trust this run does not hold", err, arm.refusal)
	}
}

func TestBuildRunExecutor_ConnectorsResolveForTheRunsTrust(t *testing.T) {
	for _, row := range []struct {
		connectorTrustArm
		trust connection.ActionTrust
		node  ir.Node
	}{
		{connectorTrustArm{"a trusted run reaches the tenant's connections", nil}, connection.TrustOfRun(store.RunTrustDefault), mutatingActionNode()},
		{connectorTrustArm{"a fork run's mutating action is refused", connection.ErrUntrustedAction}, connection.TrustOfRun(store.RunTrustFork), mutatingActionNode()},
		{connectorTrustArm{"a fork run's READ is refused too (ADR-162 decision 6)", connection.ErrUntrustedAction}, connection.TrustOfRun(store.RunTrustFork), readActionNode()},
		{connectorTrustArm{"a lineage that states no trust is refused too", connection.ErrTrustUnstated}, connection.ActionTrust{}, mutatingActionNode()},
	} {
		t.Run(row.name, func(t *testing.T) {
			s, storeDir := connectorTrustFixture(t)
			exec, err := buildRunExecutor(RunOptions{}, tiersMatchTheEngine, actionWorkflowFor(row.node), s,
				lineage{runID: "run-1", trust: row.trust}, storeDir, iterlog.Nop(), "", nil, "w", nil)
			if err != nil {
				t.Fatalf("buildRunExecutor: %v", err)
			}
			assertConnectorTrustArm(t, row.connectorTrustArm, exec, row.node)
		})
	}
}

func TestBuildResumeExecutor_ConnectorsResolveForTheRecordedTrust(t *testing.T) {
	for _, row := range []struct {
		connectorTrustArm
		run  *store.Run
		node ir.Node
	}{
		{connectorTrustArm{"a resumed trusted run reaches the tenant's connections", nil}, &store.Run{ID: "run-1", Trust: store.RunTrustDefault}, mutatingActionNode()},
		{connectorTrustArm{"a resumed fork run's mutating action is refused", connection.ErrUntrustedAction}, &store.Run{ID: "run-1", Trust: store.RunTrustFork}, mutatingActionNode()},
		{connectorTrustArm{"a resumed fork run's READ is refused too", connection.ErrUntrustedAction}, &store.Run{ID: "run-1", Trust: store.RunTrustFork}, readActionNode()},
	} {
		t.Run(row.name, func(t *testing.T) {
			s, storeDir := connectorTrustFixture(t)
			exec, err := buildResumeExecutor(ResumeOptions{RunID: "run-1"}, actionWorkflowFor(row.node), s, storeDir, iterlog.Nop(), row.run, nil)
			if err != nil {
				t.Fatalf("buildResumeExecutor: %v", err)
			}
			assertConnectorTrustArm(t, row.connectorTrustArm, exec, row.node)
		})
	}
}
