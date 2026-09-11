package gen_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// The two fixtures describe the SAME small API in the two formats iterion
// accepts. That is the point: the assertions below run against both, so a
// change that serves one format and quietly degrades the other cannot pass.
const swagger2Fixture = `{
  "swagger": "2.0",
  "info": {"title": "Probe API", "version": "9.9.9"},
  "host": "probe.example",
  "basePath": "/api/v1",
  "schemes": ["https"],
  "securityDefinitions": {
    "AuthorizationHeaderToken": {
      "type": "apiKey", "name": "Authorization", "in": "header",
      "description": "API tokens must be prepended with \"token\" followed by a space."
    },
    "BasicAuth": {"type": "basic"}
  },
  "paths": {
    "/repos/{owner}/{repo}/issues": {
      "parameters": [
        {"name": "owner", "in": "path", "required": true, "type": "string"}
      ],
      "get": {
        "tags": ["issue"], "operationId": "issueListIssues", "summary": "List issues",
        "parameters": [
          {"name": "repo", "in": "path", "required": true, "type": "string"},
          {"name": "state", "in": "query", "type": "string", "enum": ["open", "closed"]}
        ],
        "responses": {
          "200": {"description": "ok", "schema": {"type": "array", "items": {"$ref": "#/definitions/Issue"}}},
          "404": {"description": "gone"}
        }
      },
      "post": {
        "tags": ["issue"], "operationId": "issueCreateIssue", "summary": "Create an issue",
        "parameters": [
          {"name": "repo", "in": "path", "required": true, "type": "string"},
          {"name": "body", "in": "body", "schema": {"$ref": "#/definitions/CreateIssueOption"}}
        ],
        "responses": {"201": {"description": "made", "schema": {"$ref": "#/definitions/Issue"}}}
      }
    },
    "/search": {
      "post": {
        "tags": ["issue"], "operationId": "issueSearchIssues", "summary": "Search",
        "responses": {"200": {"description": "ok"}}
      }
    }
  },
  "definitions": {
    "Issue": {"type": "object", "properties": {"id": {"type": "integer"}, "title": {"type": "string"}}},
    "CreateIssueOption": {
      "type": "object", "required": ["title"],
      "properties": {"title": {"type": "string"}, "labels": {"type": "array", "items": {"type": "string"}}}
    }
  }
}`

const openapi3Fixture = `{
  "openapi": "3.0.3",
  "info": {"title": "Probe API", "version": "9.9.9"},
  "servers": [{"url": "https://probe.example/api/v1"}],
  "components": {
    "securitySchemes": {
      "bearerAuth": {"type": "http", "scheme": "bearer"}
    },
    "schemas": {
      "Issue": {"type": "object", "properties": {"id": {"type": "integer"}, "title": {"type": "string"}}},
      "CreateIssueOption": {
        "type": "object", "required": ["title"],
        "properties": {"title": {"type": "string"}, "labels": {"type": "array", "items": {"type": "string"}}}
      }
    }
  },
  "paths": {
    "/repos/{owner}/{repo}/issues": {
      "parameters": [
        {"name": "owner", "in": "path", "required": true, "schema": {"type": "string"}}
      ],
      "get": {
        "tags": ["issue"], "operationId": "issueListIssues", "summary": "List issues",
        "parameters": [
          {"name": "repo", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "state", "in": "query", "schema": {"type": "string", "enum": ["open", "closed"]}}
        ],
        "responses": {
          "200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Issue"}}}}},
          "404": {"description": "gone"}
        }
      },
      "post": {
        "tags": ["issue"], "operationId": "issueCreateIssue", "summary": "Create an issue",
        "parameters": [
          {"name": "repo", "in": "path", "required": true, "schema": {"type": "string"}}
        ],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateIssueOption"}}}
        },
        "responses": {"201": {"description": "made", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Issue"}}}}}
      }
    },
    "/search": {
      "post": {
        "tags": ["issue"], "operationId": "issueSearchIssues", "summary": "Search",
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

func generate(t *testing.T, body string) *spec.Package {
	t.Helper()
	pkg, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("the fixture must generate cleanly, skipped: %+v", report.Skipped)
	}
	return pkg
}

// TestBothFormatsAgree is the load-bearing test of the whole generator: the
// same API described as Swagger 2.0 and as OpenAPI 3 must produce the same
// operations, with the same flattened parameters — because a package's
// consumers never learn which format it came from.
func TestBothFormatsAgree(t *testing.T) {
	sw := generate(t, swagger2Fixture)
	oa := generate(t, openapi3Fixture)

	if got, want := sw.Connector.Provenance.SpecFormat, "swagger2"; got != want {
		t.Errorf("swagger format = %q, want %q", got, want)
	}
	if got, want := oa.Connector.Provenance.SpecFormat, "openapi3"; got != want {
		t.Errorf("openapi format = %q, want %q", got, want)
	}
	// Swagger's host+basePath and OpenAPI's single server URL name the same
	// origin and prefix.
	for _, tc := range []struct {
		name string
		pkg  *spec.Package
	}{{"swagger2", sw}, {"openapi3", oa}} {
		if got, want := tc.pkg.Connector.BaseURL.Default, "https://probe.example"; got != want {
			t.Errorf("%s: base default = %q, want %q", tc.name, got, want)
		}
		if got, want := tc.pkg.Connector.BaseURL.PathPrefix, "/api/v1"; got != want {
			t.Errorf("%s: base prefix = %q, want %q", tc.name, got, want)
		}
	}

	swIDs := opIDs(sw)
	oaIDs := opIDs(oa)
	if strings.Join(swIDs, ",") != strings.Join(oaIDs, ",") {
		t.Fatalf("the two formats produced different operations:\n swagger2: %v\n openapi3: %v", swIDs, oaIDs)
	}
	want := []string{"probe.issue.create_issue", "probe.issue.list_issues", "probe.issue.search_issues"}
	if strings.Join(swIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("operation ids = %v, want %v", swIDs, want)
	}

	// The body flattening is where the formats differ most (a Swagger `in:
	// body` parameter vs an OpenAPI requestBody), so it is asserted on both.
	for _, tc := range []struct {
		name string
		pkg  *spec.Package
	}{{"swagger2", sw}, {"openapi3", oa}} {
		op, ok := tc.pkg.Operation("probe.issue.create_issue")
		if !ok {
			t.Fatalf("%s: create_issue missing", tc.name)
		}
		title, ok := findParam(op, "title")
		if !ok {
			t.Fatalf("%s: the request body did not flatten into a `title` param (got %v)", tc.name, paramNames(op))
		}
		if title.In != spec.InBody {
			t.Errorf("%s: title.In = %q, want body", tc.name, title.In)
		}
		if !title.Required {
			t.Errorf("%s: title should be required (the schema lists it)", tc.name)
		}
		labels, ok := findParam(op, "labels")
		if !ok {
			t.Fatalf("%s: `labels` missing", tc.name)
		}
		if labels.Required {
			t.Errorf("%s: labels must NOT be required — a required BODY does not make every member required", tc.name)
		}
		if labels.Type != "array" || labels.Items != "string" {
			t.Errorf("%s: labels = %s of %s, want array of string", tc.name, labels.Type, labels.Items)
		}
	}
}

// TestPathItemParametersReachEveryOperation pins the shared-parameter merge.
// Dropping it would leave `{owner}` with no parameter — a request sent to a
// literally templated URL, which Validate catches but only after the cause
// has become unreadable.
func TestPathItemParametersReachEveryOperation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{{"swagger2", swagger2Fixture}, {"openapi3", openapi3Fixture}} {
		t.Run(tc.name, func(t *testing.T) {
			pkg := generate(t, tc.body)
			for _, id := range []string{"probe.issue.list_issues", "probe.issue.create_issue"} {
				op, ok := pkg.Operation(id)
				if !ok {
					t.Fatalf("%s missing", id)
				}
				owner, ok := findParam(op, "owner")
				if !ok {
					t.Fatalf("%s: the path item's shared `owner` parameter did not reach it (got %v)", id, paramNames(op))
				}
				if owner.In != spec.InPath || !owner.Required {
					t.Errorf("%s: owner = %s/required=%v, want path/required", id, owner.In, owner.Required)
				}
			}
		})
	}
}

// TestOperationParameterOverridesThePathItem pins the precedence both formats
// specify: a path item's parameters "can be overridden at the operation
// level" (OpenAPI 3.0.3, Path Item Object). Getting it backwards is silent —
// the request carries the shared declaration's type and default, and the
// override that was written to correct it loses to what it corrects.
func TestOperationParameterOverridesThePathItem(t *testing.T) {
	const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/things/{id}": {
      "parameters": [
        {"name": "id", "in": "path", "required": true, "type": "string", "description": "shared"},
        {"name": "verbose", "in": "query", "type": "string", "description": "shared only"}
      ],
      "get": {
        "tags": ["thing"], "operationId": "thingGetThing", "summary": "Get",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "type": "integer", "description": "the operation's own"}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	pkg := generate(t, body)
	op, ok := pkg.Operation("probe.thing.get_thing")
	if !ok {
		t.Fatalf("operations = %v", opIDs(pkg))
	}
	id, ok := findParam(op, "id")
	if !ok {
		t.Fatalf("`id` missing, got %v", paramNames(op))
	}
	if id.Type != "integer" || id.Description != "the operation's own" {
		t.Errorf("id = %s/%q, want the operation's own integer declaration — the path item's must not win", id.Type, id.Description)
	}
	// The shared parameter the operation did NOT override still applies:
	// precedence is an override, not a replacement of the whole list.
	if _, ok := findParam(op, "verbose"); !ok {
		t.Errorf("the shared `verbose` parameter was dropped, got %v", paramNames(op))
	}
	// Presentation order is by location then name, independent of the
	// precedence order above.
	if len(op.Params) != 2 || op.Params[0].In != spec.InPath || op.Params[1].In != spec.InQuery {
		t.Errorf("params = %v, want path before query", paramNames(op))
	}
}

// TestTheEFFECTComesFromTheMETHODAloneNeverTheNAME.
//
// This test asserted the opposite until an adversarial review broke it, and
// the reversal is the point worth keeping: a POST whose verb began with
// `get`/`search`/`check` used to be classified as a read, so that a vendor
// routing search through POST kept the retry a read is entitled to.
//
// That is unsound for the reason every text-matching guard is unsound — the
// adversary is arbitrary text. `getOrCreateLease` snake-cases to
// `get_or_create_lease`, matches the `get_` prefix, and a POST that ALLOCATES
// A LEASE became an operation iterion would blindly retry after a lost
// answer. Widening the pattern does not converge: the next vendor writes
// `lookupOrProvision`.
//
// So the derivation reads HTTP semantics and nothing else, and a genuine
// POST-search is stated in the overlay's `effect:` — one authored line, versus
// a silent duplicate.
func TestTheEffectComesFromTheMethodAloneNeverTheName(t *testing.T) {
	pkg := generate(t, swagger2Fixture)
	op, ok := pkg.Operation("probe.issue.search_issues")
	if !ok {
		t.Fatal("search_issues missing")
	}
	if op.HTTP.Method != "POST" {
		t.Fatalf("fixture changed: method = %s", op.HTTP.Method)
	}
	// Named like a read, POSTed like a mutation: the method wins.
	if op.Effect != spec.EffectCreate {
		t.Errorf("effect = %q, want create — a name must not grant retry safety", op.Effect)
	}

	create, _ := pkg.Operation("probe.issue.create_issue")
	if create.Effect != spec.EffectCreate {
		t.Errorf("create effect = %q, want create", create.Effect)
	}
	if !create.Effect.Mutating() {
		t.Error("a create must read as mutating")
	}

	// The falsifier: a GET is still a read, so the change did not simply mark
	// everything as mutating.
	get, ok := pkg.Operation("probe.issue.list_issues")
	if !ok {
		t.Fatal("list_issues missing")
	}
	if get.Effect != spec.EffectRead || get.Effect.Mutating() {
		t.Errorf("GET effect = %q, want read", get.Effect)
	}
}

// TestAuthSchemeDerivation pins two behaviours that a stored connection
// depends on: the scheme id comes from the SHAPE (so a vendor renaming its
// securityDefinitions key cannot orphan stored connections), and the prose-
// documented value prefix is recovered (missing "token " yields a 401 that
// looks like a bad credential).
func TestAuthSchemeDerivation(t *testing.T) {
	sw := generate(t, swagger2Fixture)
	tok, ok := sw.Connector.AuthScheme("token")
	if !ok {
		t.Fatalf("no `token` scheme derived, got %v", authIDs(sw))
	}
	if tok.Kind != spec.AuthAPIKey || tok.In != "header" || tok.Name != "Authorization" {
		t.Errorf("token scheme = %+v, want api_key in header Authorization", tok)
	}
	if tok.ValuePrefix != "token " {
		t.Errorf("value prefix = %q, want %q (the vendor documents it in prose only)", tok.ValuePrefix, "token ")
	}
	if _, ok := sw.Connector.AuthScheme("basic"); !ok {
		t.Errorf("no `basic` scheme derived, got %v", authIDs(sw))
	}

	oa := generate(t, openapi3Fixture)
	if _, ok := oa.Connector.AuthScheme("bearer"); !ok {
		t.Errorf("http/bearer should derive a `bearer` scheme, got %v", authIDs(oa))
	}
}

// TestGeneratedPackageAwaitsItsOverlay is the generated/authored boundary. A
// vendor description with no security scheme is normal — GitHub's own is like
// that — so generation must SUCCEED and the complete check must REFUSE, which
// is what keeps an unusable package out of a launch without making the most
// important connector ungeneratable.
func TestGeneratedPackageAwaitsItsOverlay(t *testing.T) {
	body := strings.Replace(swagger2Fixture,
		`"securityDefinitions": {
    "AuthorizationHeaderToken": {
      "type": "apiKey", "name": "Authorization", "in": "header",
      "description": "API tokens must be prepended with \"token\" followed by a space."
    },
    "BasicAuth": {"type": "basic"}
  },`, "", 1)
	if body == swagger2Fixture {
		t.Fatal("fixture edit did not apply — the test would prove nothing")
	}

	pkg, _, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("generation must succeed without a security scheme: %v", err)
	}
	if len(pkg.Connector.Auth) != 0 {
		t.Fatalf("auth = %v, want none", authIDs(pkg))
	}
	if err := pkg.ValidateGenerated(); err != nil {
		t.Errorf("the generated half must be valid: %v", err)
	}
	err = pkg.Validate()
	if err == nil {
		t.Fatal("the complete check must refuse a package with no auth scheme")
	}
	if !strings.Contains(err.Error(), "auth scheme") {
		t.Errorf("the refusal must name what is missing, got: %v", err)
	}
}

// TestOneMalformedOperationIsSkippedNotFatal is the choice a real vendor
// description forces. GitLab's auto-generated OpenAPI declares a path
// parameter `issue_id` on a path templated `{epic_issue_id}`; failing the
// generation there would lose its other ~1200 operations to that one — the
// shape of the broken manifest that failed every launch of a team for 2h22
// (ADR-080's amendment). The skip must reach the caller, though: a catalog
// with invisible holes is the other way to be wrong.
func TestOneMalformedOperationIsSkippedNotFatal(t *testing.T) {
	const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/good/{id}": {
      "get": {
        "tags": ["thing"], "operationId": "thingGetThing", "summary": "Fine",
        "parameters": [{"name": "id", "in": "path", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/bad/{epic_issue_id}": {
      "get": {
        "tags": ["thing"], "operationId": "thingGetBad", "summary": "Mismatched",
        "parameters": [{"name": "issue_id", "in": "path", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	pkg, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("one malformed operation must not fail the generation: %v", err)
	}
	if _, ok := pkg.Operation("probe.thing.get_thing"); !ok {
		t.Errorf("the good operation was lost, got %v", opIDs(pkg))
	}
	if _, ok := pkg.Operation("probe.thing.get_bad"); ok {
		t.Error("the malformed operation must not be in the package")
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("report.Skipped = %+v, want exactly the one bad operation", report.Skipped)
	}
	skip := report.Skipped[0]
	if skip.Path != "/bad/{epic_issue_id}" || skip.Method != "GET" {
		t.Errorf("the skip must locate the operation in the description, got %+v", skip)
	}
	if skip.SourceOperationID != "thingGetBad" {
		t.Errorf("the skip must name the vendor's own id, got %q", skip.SourceOperationID)
	}
	if !strings.Contains(skip.Reason, "issue_id") {
		t.Errorf("the reason must name what is wrong, got %q", skip.Reason)
	}
	// A skipped id must not stay reserved: the next operation deriving the
	// same name would otherwise be pushed to `…_2` by a ghost.
	if _, ok := pkg.Operation("probe.thing.get_bad_2"); ok {
		t.Error("a skipped operation left its id reserved")
	}
}

func TestRejectsUnknownFormats(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no version marker", `{"paths": {}}`, "not an API description"},
		{"swagger 1.2", `{"swagger": "1.2", "paths": {}}`, "unsupported swagger version"},
		{"openapi 4", `{"openapi": "4.0.0", "paths": {}}`, "unsupported openapi version"},
		{"no path", `{"swagger": "2.0", "paths": {}}`, "declares no path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := gen.Generate([]byte(tc.body), gen.Options{ConnectorID: "probe"})
			if err == nil {
				t.Fatalf("want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestYAMLDescriptionParses(t *testing.T) {
	const y = `
swagger: "2.0"
info:
  title: Probe
  version: "1.0"
host: probe.example
basePath: /api
securityDefinitions:
  tok:
    type: apiKey
    name: X-Token
    in: header
paths:
  /things/{id}:
    get:
      tags: [thing]
      operationId: thingGetThing
      summary: Get a thing
      parameters:
        - name: id
          in: path
          required: true
          type: string
      responses:
        "200":
          description: ok
`
	pkg, _, err := gen.Generate([]byte(y), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("generate from YAML: %v", err)
	}
	if _, ok := pkg.Operation("probe.thing.get_thing"); !ok {
		t.Errorf("operations = %v, want probe.thing.get_thing", opIDs(pkg))
	}
}

// TestDeterministicOutput pins reproducibility: regenerating an unchanged
// description must produce an identical package, or every regeneration is an
// unreviewable diff.
func TestDeterministicOutput(t *testing.T) {
	fixed := func() spec.Maturity { return spec.MaturityExperimental }()
	a, _, err := gen.Generate([]byte(swagger2Fixture), gen.Options{ConnectorID: "probe", Maturity: fixed})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := gen.Generate([]byte(swagger2Fixture), gen.Options{ConnectorID: "probe", Maturity: fixed})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(opIDs(a), ",") != strings.Join(opIDs(b), ",") {
		t.Error("two generations of the same description disagree on operation order")
	}
	for i := range a.Ops {
		if a.Ops[i].Domain != b.Ops[i].Domain {
			t.Errorf("domain %d: %q vs %q", i, a.Ops[i].Domain, b.Ops[i].Domain)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func opIDs(p *spec.Package) []string {
	ops := p.Operations()
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.ID)
	}
	return out
}

func authIDs(p *spec.Package) []string {
	out := make([]string, 0, len(p.Connector.Auth))
	for _, a := range p.Connector.Auth {
		out = append(out, a.ID)
	}
	return out
}

func paramNames(op spec.Operation) []string {
	out := make([]string, 0, len(op.Params))
	for _, p := range op.Params {
		out = append(out, string(p.In)+":"+p.Name)
	}
	return out
}

func findParam(op spec.Operation, name string) (spec.Param, bool) {
	for _, p := range op.Params {
		if p.Name == name {
			return p, true
		}
	}
	return spec.Param{}, false
}

// TestWhatCannotBeDerivedIsREPORTEDNotErased covers the gap that made a
// package's coverage claim untrustworthy.
//
// The generator dropped what it could not express — a parameter in a location
// it cannot build, a request body whose `$ref` the description never defines —
// and published the operation anyway with ZERO skips reported. Both produce an
// operation that looks perfectly callable and cannot be called: one is missing
// an input the vendor requires, the other sends no payload where a payload is
// mandatory. An invisible hole is precisely what the skip mechanism exists to
// make impossible, so silence there defeated it.
func TestWhatCannotBeDerivedIsReportedNotErased(t *testing.T) {
	t.Run("a required parameter in a location iterion cannot build", func(t *testing.T) {
		const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/thing": {
      "get": {
        "tags": ["thing"], "operationId": "thingGetThing", "summary": "Needs a cookie",
        "parameters": [{"name": "session", "in": "cookie", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
		_, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
		// Nothing could be derived, so the generation fails — but it must fail
		// having COUNTED the loss, not silently.
		if err == nil {
			t.Fatal("a description whose only operation is underivable must fail")
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("report.Skipped = %+v, want the operation counted as a gap", report.Skipped)
		}
		if !strings.Contains(report.Skipped[0].Reason, "session") {
			t.Errorf("the reason must name the parameter that was lost: %q", report.Skipped[0].Reason)
		}
	})

	// The SAME parameter, delivered by reference — the form a real description
	// uses for anything shared. `boolAt(pm, "required")` was read off the
	// `{"$ref": …}` wrapper, which carries nothing else, so it was always
	// false: the two cases the check names were exactly the two it could not
	// see, and the operation shipped missing an input the vendor requires with
	// ZERO skips reported.
	t.Run("a required parameter delivered as a $ref", func(t *testing.T) {
		const body = `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "servers": [{"url": "https://probe.example"}],
  "components": {
    "securitySchemes": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
    "parameters": {
      "Session": {"name": "session", "in": "cookie", "required": true, "schema": {"type": "string"}}
    }
  },
  "paths": {
    "/thing": {
      "get": {
        "tags": ["thing"], "operationId": "thingGetThing", "summary": "Needs a cookie",
        "parameters": [{"$ref": "#/components/parameters/Session"}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
		_, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
		if err == nil {
			t.Fatal("a description whose only operation is underivable must fail")
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("report.Skipped = %+v, want the operation counted as a gap", report.Skipped)
		}
		if !strings.Contains(report.Skipped[0].Reason, "session") {
			t.Errorf("the reason must name the parameter that was lost: %q", report.Skipped[0].Reason)
		}
	})

	// A reference this document does not define says NOTHING about what it
	// declared — including whether it was required — so it cannot be treated
	// as optional: that is a guess in the direction that ships a hole.
	t.Run("a parameter whose $ref is not defined", func(t *testing.T) {
		const body = `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "servers": [{"url": "https://probe.example"}],
  "components": {"securitySchemes": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}}},
  "paths": {
    "/thing": {
      "get": {
        "tags": ["thing"], "operationId": "thingGetThing", "summary": "Needs something",
        "parameters": [{"$ref": "shared.yaml#/components/parameters/Session"}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
		_, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
		if err == nil {
			t.Fatal("an operation whose parameter cannot be read must not be published")
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("report.Skipped = %+v, want the operation counted as a gap", report.Skipped)
		}
		if !strings.Contains(report.Skipped[0].Reason, "shared.yaml") {
			t.Errorf("the reason must name the reference it could not read: %q", report.Skipped[0].Reason)
		}
	})

	t.Run("a request body whose $ref is not defined", func(t *testing.T) {
		const body = `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "servers": [{"url": "https://probe.example"}],
  "components": {"securitySchemes": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}}},
  "paths": {
    "/good": {
      "get": {
        "tags": ["thing"], "operationId": "thingGetThing", "summary": "Fine",
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/thing": {
      "post": {
        "tags": ["thing"], "operationId": "thingMakeThing", "summary": "Needs a body",
        "requestBody": {"$ref": "#/components/requestBodies/NeverDefined"},
        "responses": {"201": {"description": "created"}}
      }
    }
  }
}`
		pkg, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
		if err != nil {
			t.Fatalf("the sound operation must survive: %v", err)
		}
		if _, ok := pkg.Operation("probe.thing.make_thing"); ok {
			t.Error("an operation whose body could not be derived must not be published — it would send no payload at all")
		}
		if _, ok := pkg.Operation("probe.thing.get_thing"); !ok {
			t.Error("the sound operation was lost with the unsound one")
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("report.Skipped = %+v, want the body-less operation counted", report.Skipped)
		}
		if !strings.Contains(report.Skipped[0].Reason, "NeverDefined") {
			t.Errorf("the reason must name the reference that could not be resolved: %q", report.Skipped[0].Reason)
		}
	})
}

// TestAREQUIREDWholeBodyStaysRequired.
//
// `required: true` on a requestBody is not propagated onto its MEMBERS — that
// is correct, since which members are mandatory is the schema's own list. But
// a WHOLE-BODY parameter IS the envelope, and leaving it optional published an
// operation whose mandatory payload could simply be omitted: the request went
// out with no body at all, and the local required-parameter check that exists
// to catch exactly this had nothing to check.
func TestARequiredWholeBodyStaysRequired(t *testing.T) {
	const body = `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "servers": [{"url": "https://probe.example"}],
  "components": {"securitySchemes": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}}},
  "paths": {
    "/things": {
      "post": {
        "tags": ["thing"], "operationId": "thingReplaceAll", "summary": "Replace",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"type": "array", "items": {"type": "integer"}}}}
        },
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/optional": {
      "post": {
        "tags": ["thing"], "operationId": "thingMaybe", "summary": "Maybe",
        "requestBody": {
          "content": {"application/json": {"schema": {"type": "array", "items": {"type": "integer"}}}}
        },
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	pkg := generate(t, body)

	op, ok := pkg.Operation("probe.thing.replace_all")
	if !ok {
		t.Fatalf("replace_all missing, got %v", opIDs(pkg))
	}
	p, ok := findParam(op, "body")
	if !ok {
		t.Fatalf("the whole-body param is missing: %v", paramNames(op))
	}
	if !p.WholeBody {
		t.Error("a body schema with no properties IS the body")
	}
	if !p.Required {
		t.Error("the envelope was declared required, so the parameter that IS the envelope must be")
	}

	// The falsifier: an optional body stays optional, so this did not simply
	// mark every whole body required.
	opt, _ := pkg.Operation("probe.thing.maybe")
	q, ok := findParam(opt, "body")
	if !ok {
		t.Fatalf("the optional whole-body param is missing: %v", paramNames(opt))
	}
	if q.Required {
		t.Error("a body the vendor did not declare required must stay optional")
	}
}

// TestARequiredWholeBodyStaysRequiredInSWAGGER2.
//
// The sibling of the test above, on the other ingest. The OpenAPI 3 branch
// propagated `required` onto a whole-body parameter and the Swagger 2 branch
// did not — same function, one arm — so a Swagger 2 operation whose body IS
// the payload published it as optional. `checkParams` then treats the unset
// optional as absent and `buildBody` returns nil: the POST goes out with NO
// body, no local refusal, and the vendor 400s for a reason the workflow cannot
// read. Both of the specs this generator was measured on (Slack, Forgejo) are
// Swagger 2.0, and `render_markdown_raw` — whose body is the document to
// render — is exactly this shape in the shipped package.
func TestARequiredWholeBodyStaysRequiredInSwagger2(t *testing.T) {
	const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "basePath": "/api/v1",
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/markdown/raw": {
      "post": {
        "tags": ["thing"], "operationId": "thingRenderRaw", "summary": "Render",
        "consumes": ["application/json"],
        "parameters": [
          {"name": "body", "in": "body", "required": true, "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/optional": {
      "post": {
        "tags": ["thing"], "operationId": "thingMaybe", "summary": "Maybe",
        "consumes": ["application/json"],
        "parameters": [
          {"name": "body", "in": "body", "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	pkg := generate(t, body)

	op, ok := pkg.Operation("probe.thing.render_raw")
	if !ok {
		t.Fatalf("render_raw missing, got %v", opIDs(pkg))
	}
	p, ok := findParam(op, "body")
	if !ok {
		t.Fatalf("the whole-body param is missing: %v", paramNames(op))
	}
	if !p.WholeBody {
		t.Error("a body schema with no properties IS the body")
	}
	if !p.Required {
		t.Error("the vendor declared the body required, so the parameter that IS the body must be")
	}

	// The falsifier: an optional body stays optional.
	opt, _ := pkg.Operation("probe.thing.maybe")
	q, ok := findParam(opt, "body")
	if !ok {
		t.Fatalf("the optional whole-body param is missing: %v", paramNames(opt))
	}
	if q.Required {
		t.Error("a body the vendor did not mark required must stay optional")
	}
}

// TestASwaggerBodyItCannotBuildIsAGapNotJSON.
//
// `swaggerBodyEncoding` walked `consumes` for a media type it could build and
// fell through to JSON — the format's default for an operation that declares
// NO consumes at all, but also, wrongly, the answer when every declared type
// is one iterion cannot build. The operation then shipped with `request_body:
// json`, and a whole-body `type: string` went out as `json.Marshal("# Hello")`
// — quotes included — under `Content-Type: application/json`.
//
// The OpenAPI 3 arm refuses exactly this and names the media types, which
// validation turns into a coverage gap. Both pilot specs are Swagger 2.0, so
// the asymmetry sat on the ingest actually in use.
func TestASwaggerBodyItCannotBuildIsAGapNotJSON(t *testing.T) {
	const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/markdown/raw": {
      "post": {
        "tags": ["thing"], "operationId": "thingRenderRaw", "summary": "Render",
        "consumes": ["text/plain"],
        "parameters": [{"name": "body", "in": "body", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	_, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
	if err == nil {
		t.Fatal("a description whose only operation has an unbuildable body must fail rather than publish it")
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("report.Skipped = %+v, want the operation counted as a gap", report.Skipped)
	}
	// The reason must name WHICH encoding was refused, or the gap cannot be
	// acted on — an overlay is how it gets fixed.
	if !strings.Contains(report.Skipped[0].Reason, "text/plain") {
		t.Errorf("the reason must name the media type it could not build: %q", report.Skipped[0].Reason)
	}
}

// The falsifier: NO `consumes` anywhere is the format's own default, and that
// really is JSON. Refusing it would turn most of a Swagger description into
// coverage gaps.
func TestASwaggerBodyWithNoConsumesIsStillJSON(t *testing.T) {
	const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/things": {
      "post": {
        "tags": ["thing"], "operationId": "thingCreate", "summary": "Create",
        "parameters": [{"name": "body", "in": "body", "schema": {"type": "object", "properties": {"title": {"type": "string"}}}}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	pkg := generate(t, body)
	op, ok := pkg.Operation("probe.thing.create")
	if !ok {
		t.Fatalf("create missing, got %v", opIDs(pkg))
	}
	if op.HTTP.RequestBody != spec.BodyJSON {
		t.Errorf("request body = %q, want json — the format's default with no consumes declared", op.HTTP.RequestBody)
	}
}
