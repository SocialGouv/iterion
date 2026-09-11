package overlay_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// The fixture is GitHub's shape in miniature: an otherwise excellent
// description that declares NO security scheme, so the generated half cannot
// pass the complete check on its own. That is the case the split exists for.
const noAuthFixture = `{
  "openapi": "3.0.3",
  "info": {"title": "Probe API", "version": "1.0"},
  "servers": [{"url": "https://probe.example/api/v3"}],
  "components": {
    "schemas": {"Issue": {"type": "object", "properties": {"id": {"type": "integer"}}}}
  },
  "paths": {
    "/repos/{owner}/{repo}/issues": {
      "get": {
        "tags": ["issue"], "operationId": "issueList", "summary": "List issues",
        "parameters": [
          {"name": "owner", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "repo", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "page", "in": "query", "schema": {"type": "integer"}},
          {"name": "per_page", "in": "query", "schema": {"type": "integer"}}
        ],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Issue"}}}}}}
      },
      "post": {
        "tags": ["issue"], "operationId": "issueCreate", "summary": "Create an issue",
        "parameters": [
          {"name": "owner", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "repo", "in": "path", "required": true, "schema": {"type": "string"}}
        ],
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "required": ["title"], "properties": {"title": {"type": "string"}}}}}},
        "responses": {"201": {"description": "made", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Issue"}}}}}
      }
    },
    "/search/issues": {
      "get": {
        "tags": ["search"], "operationId": "searchIssues", "summary": "Search",
        "parameters": [{"name": "q", "in": "query", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

func generated(t *testing.T) *spec.Package {
	t.Helper()
	pkg, report, err := gen.Generate([]byte(noAuthFixture), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("the fixture must generate cleanly, skipped: %+v", report.Skipped)
	}
	return pkg
}

// TestTheTwoHalvesNeedEachOther is the property the whole generated/authored
// split rests on: neither half is usable alone, and the merge is where the
// complete check runs.
func TestTheTwoHalvesNeedEachOther(t *testing.T) {
	pkg := generated(t)
	if err := pkg.ValidateGenerated(); err != nil {
		t.Fatalf("the generated half must be structurally valid: %v", err)
	}
	if err := pkg.Validate(); err == nil {
		t.Fatal("a package with no auth must NOT pass the complete check on its own")
	}

	ov := &overlay.Overlay{
		Connector: "probe",
		Auth: []spec.AuthScheme{{
			ID: "token", Kind: spec.AuthBearer, DisplayName: "Personal access token",
		}},
	}
	if err := overlay.Apply(pkg, ov); err != nil {
		t.Fatalf("the merged package must validate: %v", err)
	}
	if _, ok := pkg.Connector.AuthScheme("token"); !ok {
		t.Error("the overlay's auth scheme did not reach the package")
	}
}

// TestOverlayCarriesWhatNoSpecCanState walks the list this package documents,
// on one operation, so a regression in any single field is named.
func TestOverlayCarriesWhatNoSpecCanState(t *testing.T) {
	pkg := generated(t)
	explode := false
	yes, no := true, false
	ov := &overlay.Overlay{
		Connector: "probe",
		Auth:      []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
		Operations: map[string]overlay.OperationOverlay{
			"probe.issue.list": {
				ID:      "probe.issue.list_for_repo", // the identity pin
				Summary: "List a repository's issues",
				MCP:     &yes,
				Pagination: &spec.Pagination{
					Style: spec.PageNumber, PageParam: "page", SizeParam: "per_page",
					DefaultSize: 50, MaxPages: 20,
				},
				Params: map[string]overlay.ParamOverlay{
					"per_page": {Key: "page_size", Description: "Items per page"},
				},
			},
			"probe.search.issues": {
				// A GET that the derivation already reads as a read; what an
				// overlay adds here is the serialization the description left
				// implicit and a rename for the vendor's cryptic `q`.
				Params: map[string]overlay.ParamOverlay{
					"q": {Key: "query", Style: spec.StyleForm, Explode: &explode},
				},
				MCP: &yes,
			},
			"probe.issue.create": {
				MCP:                 &no,
				IdempotencyKeyParam: "title",
			},
		},
	}
	if err := overlay.Apply(pkg, ov); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The id pin took, and the old id is gone.
	if _, ok := pkg.Operation("probe.issue.list_for_repo"); !ok {
		t.Fatalf("the pinned id is missing, got %v", ids(pkg))
	}
	if _, ok := pkg.Operation("probe.issue.list"); ok {
		t.Error("the derived id survived the pin")
	}

	list, _ := pkg.Operation("probe.issue.list_for_repo")
	if list.Pagination == nil || list.Pagination.Style != spec.PageNumber || list.Pagination.MaxPages != 20 {
		t.Errorf("pagination = %+v, want the overlay's page-number walk", list.Pagination)
	}
	if !list.MCP {
		t.Error("the operation was not put in the curated MCP set")
	}
	// A key rename must not touch the wire name: the vendor still receives
	// `per_page`, the `.bot` author writes `page_size`.
	renamed, ok := paramByKey(list, "page_size")
	if !ok {
		t.Fatalf("the key rename did not take, keys = %v", paramKeys(list))
	}
	if renamed.Name != "per_page" {
		t.Errorf("wire name = %q, want per_page unchanged", renamed.Name)
	}

	search, _ := pkg.Operation("probe.search.issues")
	q, ok := paramByKey(search, "query")
	if !ok {
		t.Fatalf("the `q` rename did not take, keys = %v", paramKeys(search))
	}
	if q.Name != "q" || q.ExplodeOrDefault() {
		t.Errorf("q = name %q explode %v, want the wire name kept and explode false", q.Name, q.ExplodeOrDefault())
	}

	create, _ := pkg.Operation("probe.issue.create")
	if create.MCP {
		t.Error("an operation an overlay excluded is in the curated set")
	}
	if create.IdempotencyKeyParam != "title" {
		t.Errorf("idempotency param = %q, want title", create.IdempotencyKeyParam)
	}
}

// TestAnEntryThatMatchesNothingIsAnError is the guard that makes the overlay
// survive a regeneration. Ignoring a stale entry would bring its operation
// back UNCORRECTED — an unmarked credential, a list that no longer paginates —
// while the overlay still looked applied.
func TestAnEntryThatMatchesNothingIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ov      *overlay.Overlay
		wantMsg string
	}{
		{
			name: "an operation the package does not have",
			ov: &overlay.Overlay{
				Connector:  "probe",
				Auth:       []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
				Operations: map[string]overlay.OperationOverlay{"probe.issue.moved_away": {Summary: "x"}},
			},
			wantMsg: "probe.issue.moved_away",
		},
		{
			name: "a drop that matches nothing",
			ov: &overlay.Overlay{
				Connector: "probe",
				Auth:      []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
				Drop:      []string{"probe.issue.gone"},
			},
			wantMsg: "probe.issue.gone",
		},
		{
			name: "a parameter the operation does not have",
			ov: &overlay.Overlay{
				Connector: "probe",
				Auth:      []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
				Operations: map[string]overlay.OperationOverlay{
					"probe.issue.list": {Params: map[string]overlay.ParamOverlay{"nope": {Key: "x"}}},
				},
			},
			wantMsg: "nope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := overlay.Apply(generated(t), tc.ov)
			if err == nil {
				t.Fatalf("want a refusal naming %q", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %v, want it to name %q", err, tc.wantMsg)
			}
		})
	}
}

// TestDropRemovesTheOperation pins the escape hatch for an endpoint the
// catalog should not carry.
func TestDropRemovesTheOperation(t *testing.T) {
	pkg := generated(t)
	before := len(pkg.Operations())
	err := overlay.Apply(pkg, &overlay.Overlay{
		Connector: "probe",
		Auth:      []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
		Drop:      []string{"probe.search.issues"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := pkg.Operation("probe.search.issues"); ok {
		t.Error("the dropped operation is still in the package")
	}
	if got := len(pkg.Operations()); got != before-1 {
		t.Errorf("operations = %d, want %d", got, before-1)
	}
}

// TestDeterminismCanOnlyBeRemoved pins the one-way door. An overlay knows
// things a generator cannot — that an endpoint runs a model — but letting it
// assert determinism BACK would let a human overrule a structural refusal
// with a promise, which is what this catalog must not accept on trust.
func TestDeterminismCanOnlyBeRemoved(t *testing.T) {
	off, on := false, true

	pkg := generated(t)
	if err := overlay.Apply(pkg, &overlay.Overlay{
		Connector:  "probe",
		Auth:       []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
		Operations: map[string]overlay.OperationOverlay{"probe.search.issues": {Deterministic: &off}},
	}); err != nil {
		t.Fatalf("removing the claim must be allowed: %v", err)
	}
	op, _ := pkg.Operation("probe.search.issues")
	if op.Deterministic {
		t.Error("the overlay did not remove the determinism claim")
	}

	err := overlay.Apply(generated(t), &overlay.Overlay{
		Connector:  "probe",
		Auth:       []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
		Operations: map[string]overlay.OperationOverlay{"probe.search.issues": {Deterministic: &on}},
	})
	if err == nil {
		t.Fatal("asserting determinism must be refused")
	}
	if !strings.Contains(err.Error(), "only REMOVE") {
		t.Errorf("refusal = %v, want it to say the claim may only be removed", err)
	}
}

// TestOverlayForTheWrongPackageIsRefused prevents the silent case: applied to
// the wrong package, every entry would match nothing and correct nothing.
func TestOverlayForTheWrongPackageIsRefused(t *testing.T) {
	err := overlay.Apply(generated(t), &overlay.Overlay{Connector: "somethingelse"})
	if err == nil {
		t.Fatal("an overlay naming another connector must be refused")
	}
	if !strings.Contains(err.Error(), "somethingelse") {
		t.Errorf("refusal = %v, want it to name the mismatch", err)
	}
}

// TestVersionIsReadBeforeTheStrictParse mirrors the package loader's own
// order, for the same reason: a document written for a newer iterion must say
// so instead of failing on whichever unknown field comes first.
func TestVersionIsReadBeforeTheStrictParse(t *testing.T) {
	_, err := overlay.Parse([]byte("schema_version: 99\nconnector: probe\nsomething_new: true\n"))
	if err == nil {
		t.Fatal("a newer schema must be refused")
	}
	if !strings.Contains(err.Error(), "upgrade iterion") {
		t.Errorf("refusal = %v, want it to name the version", err)
	}

	_, err = overlay.Parse([]byte("schema_version: 1\nconnector: probe\ntypo_here: true\n"))
	if err == nil {
		t.Fatal("an unknown field at a supported version must be refused")
	}
	if strings.Contains(err.Error(), "upgrade iterion") {
		t.Errorf("a typo must not be reported as a version problem: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func ids(p *spec.Package) []string {
	ops := p.Operations()
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.ID)
	}
	return out
}

func paramKeys(op spec.Operation) []string {
	out := make([]string, 0, len(op.Params))
	for _, p := range op.Params {
		out = append(out, p.Key)
	}
	return out
}

func paramByKey(op spec.Operation, key string) (spec.Param, bool) {
	for _, p := range op.Params {
		if p.Key == key {
			return p, true
		}
	}
	return spec.Param{}, false
}
