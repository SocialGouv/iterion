package cli_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestTheStudioConnectorFloorHoldsNoWorkspaceTier: the process-wide catalog a
// local studio falls back to is the trust-independent floor — the operator's
// home tier and the baked packages — and never a workspace tier rooted at
// whatever directory the studio was started in. The workspace tier is exactly
// the tier a run's own code can plant packages in, so it opens for THE RUN
// whose workspace it is (its grant carries the directory to the per-run
// builder) and for no other.
//
// Mutation that reddens this: root the floor at `workspace` again (pass it as
// the project root of the fallback catalog), or read os.Getwd inside
// LocalConnectorPlane.
func TestTheStudioConnectorFloorHoldsNoWorkspaceTier(t *testing.T) {
	// Granted from the start: the floor must hold no workspace tier because
	// it roots none, not because a grant closed it.
	t.Setenv(connection.ProjectCatalogEnv, "1")
	cwd := t.TempDir()
	home := t.TempDir()
	// A project tier EXISTS at the launch directory — the exact state the
	// cwd-rooted catalog served to every run.
	writePackageAs(t, filepath.Join(cwd, "connectors"), "shadow", "DELETE", "/repos/{owner}/{repo}", spec.EffectRead)

	floor, builder := cli.LocalConnectorPlane(cwd, home)

	got := floor.Connectors()
	for _, id := range got {
		if id == "shadow" {
			t.Errorf("the fallback catalog serves %v — the launch directory's workspace tier leaked into it", got)
		}
	}
	if !reflect.DeepEqual(got, connection.BakedCatalog().Connectors()) {
		t.Errorf("the floor = %v, want exactly the baked packages' set", got)
	}

	// And the same directory through the BUILDER with a trust that does not
	// own the code: still nothing — the tier opens for the run whose
	// workspace it is and whose trust says so.
	perRun, err := builder(cwd, connection.TrustUnknown())
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	for _, id := range perRun.Connectors() {
		if id == "shadow" {
			t.Errorf("an unknown-trust run was served the workspace tier: %v", perRun.Connectors())
		}
	}
}

// TestTheStudioBuilderServesTheRunsOwnWorkspace: the per-run builder roots
// the project tier at the workDir it is handed — the run's own — and applies
// the run's trust to it, so a trusted run over its own checkout is served
// while a fork run is not.
//
// Mutation that reddens this: build the per-run paths from a fixed root (the
// home root, the launch directory) instead of workDir.
func TestTheStudioBuilderServesTheRunsOwnWorkspace(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writePackageAs(t, filepath.Join(workspace, "connectors"), "shadow", "DELETE", "/repos/{owner}/{repo}", spec.EffectDelete)

	_, builder := cli.LocalConnectorPlane(t.TempDir(), home)
	t.Setenv(connection.ProjectCatalogEnv, "1")

	owned, err := builder(workspace, connection.TrustOfRun(store.RunTrustDefault))
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	has := false
	for _, id := range owned.Connectors() {
		if id == "shadow" {
			has = true
		}
	}
	if !has {
		t.Errorf("a trusted run over its own checkout got %v — its workspace tier is missing", owned.Connectors())
	}

	fork, err := builder(workspace, connection.TrustOfRun(store.RunTrustFork))
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	for _, id := range fork.Connectors() {
		if id == "shadow" {
			t.Errorf("a fork run was served the workspace tier: %v", fork.Connectors())
		}
	}
}

// writePackageAs plants a minimal valid package under root/id — the same
// shape the tier tests in pkg/connection use, small enough to read whole.
func writePackageAs(t *testing.T, root, id, method, path string, effect spec.Effect) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := spec.Write(dir, &spec.Package{
		Connector: spec.Connector{
			ID: id, Version: "1.0.0", Maturity: spec.MaturityExperimental,
			BaseURL: spec.BaseURL{Default: "https://example.invalid"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization",
			}},
		},
		Ops: []spec.OpsFile{{Operations: []spec.Operation{{
			ID:       id + ".repo.purge",
			Resource: "repo", Verb: "purge",
			HTTP:   spec.HTTPBinding{Method: method, Path: path},
			Effect: effect, Deterministic: true,
			Params:   []spec.Param{{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true}, {Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true}},
			Results:  []spec.ResultCase{{Status: 204}},
			Security: []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{{SchemeID: "token"}}}},
		}}}},
	}); err != nil {
		t.Fatalf("write package: %v", err)
	}
}
