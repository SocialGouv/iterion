package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// A two-operation Swagger description, enough to carry an overlay that renames
// an id — which is the shape the idempotence defect needed.
const genFixture = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "basePath": "/api/v1",
  "schemes": ["https"],
  "securityDefinitions": {
    "AuthorizationHeaderToken": {"type": "apiKey", "name": "Authorization", "in": "header"}
  },
  "security": [{"AuthorizationHeaderToken": []}],
  "paths": {
    "/issues": {
      "get": {
        "tags": ["issue"], "operationId": "issueListIssues", "summary": "List",
        "responses": {"200": {"description": "ok"}}
      },
      "post": {
        "tags": ["issue"], "operationId": "issueCreateIssue", "summary": "Create",
        "responses": {"201": {"description": "created"}}
      }
    }
  }
}`

const genOverlay = `schema_version: 1
connector: probe

operations:
  probe.issue.list_issues:
    id: probe.issue.list
    mcp: true
`

// TestRegeneratingIsIDEMPOTENT is the guard for the defect that made the
// documented workflow — regenerate, then validate — fail on the shipped
// package.
//
// `gen` applied the overlay to the package it then WROTE, so ops/ landed with
// the overlay's renamed ids already in place. The next `validate` loads ops/
// and applies the overlay again, looking up the ORIGINAL ids: none of them
// exist any more, and the run fails on "unmatched" entries the operator never
// touched. It also broke the contract the two halves rest on — ops/ is a pure
// derivation of the vendor's description, and an overlay baked into it is not.
//
// The test asserts the loop an operator actually performs: generate, validate,
// generate again, validate again.
func TestRegeneratingIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "probe.json")
	if err := os.WriteFile(specPath, []byte(genFixture), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	outDir := filepath.Join(dir, "probe")

	gen := func() string {
		t.Helper()
		var buf bytes.Buffer
		err := cli.ConnectorsGen(cli.ConnectorsGenOptions{
			Spec: specPath, ID: "probe", Out: outDir,
			Version: "0.1.0", License: "MIT", Redistributable: true,
			KeepOverlay: true,
		}, &buf)
		if err != nil {
			t.Fatalf("gen: %v\n%s", err, buf.String())
		}
		return buf.String()
	}
	validate := func() string {
		t.Helper()
		var buf bytes.Buffer
		if err := cli.ConnectorsValidate(outDir, &buf); err != nil {
			t.Fatalf("validate: %v\n%s", err, buf.String())
		}
		return buf.String()
	}

	// 1. First generation, with no overlay yet.
	gen()
	// 2. The operator writes the authored half, renaming an id.
	if err := os.WriteFile(filepath.Join(outDir, "overlay.yaml"), []byte(genOverlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	out := validate()
	if !strings.Contains(out, "probe") {
		t.Fatalf("validate said nothing useful: %s", out)
	}

	// 3. The vendor publishes a new description; the operator regenerates.
	gen()

	// 4. And validates again. This is where it used to fail: the written ops/
	//    already carried `probe.issue.list`, so the overlay's key
	//    `probe.issue.list_issues` matched nothing.
	validate()

	// The written half must still hold the DERIVED id, never the overlay's.
	// Reading it back is what proves ops/ stayed a pure derivation rather than
	// a merged artifact that merely happens to validate.
	generated, err := spec.LoadGenerated(outDir)
	if err != nil {
		t.Fatalf("load generated: %v", err)
	}
	if _, ok := generated.Operation("probe.issue.list_issues"); !ok {
		t.Error("ops/ must hold the DERIVED id — an overlay baked into the generated half makes it non-regenerable")
	}
	if _, ok := generated.Operation("probe.issue.list"); ok {
		t.Error("ops/ carries the overlay's renamed id: the two halves are no longer separable")
	}
}

// TestAStaleOverlayIsRefusedLOUDLY is the other half: the check that used to
// live in the same place still has to bite. An overlay naming an operation the
// regenerated package no longer has is a correction that has silently stopped
// applying, which is worse than an error.
func TestAStaleOverlayIsRefusedLoudly(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "probe.json")
	if err := os.WriteFile(specPath, []byte(genFixture), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	outDir := filepath.Join(dir, "probe")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := `schema_version: 1
connector: probe

operations:
  probe.issue.vanished:
    summary: an operation the description no longer describes
`
	if err := os.WriteFile(filepath.Join(outDir, "overlay.yaml"), []byte(stale), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	var buf bytes.Buffer
	err := cli.ConnectorsGen(cli.ConnectorsGenOptions{
		Spec: specPath, ID: "probe", Out: outDir,
		Version: "0.1.0", License: "MIT", Redistributable: true,
		KeepOverlay: true,
	}, &buf)
	if err == nil {
		t.Fatal("an overlay naming an operation that no longer exists must fail the regeneration")
	}
	if !strings.Contains(err.Error(), "vanished") {
		t.Errorf("the refusal must name the entry that no longer applies: %v", err)
	}
}
