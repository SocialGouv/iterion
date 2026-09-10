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
	pkg, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("generate: %v", err)
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

// TestSearchUnderPostIsARead pins the one correction the verb makes to the
// method. Vendors route search through POST when the query outgrows a URL;
// classifying those as `create` would deny them the retry a read is entitled
// to.
func TestSearchUnderPostIsARead(t *testing.T) {
	pkg := generate(t, swagger2Fixture)
	op, ok := pkg.Operation("probe.issue.search_issues")
	if !ok {
		t.Fatal("search_issues missing")
	}
	if op.HTTP.Method != "POST" {
		t.Fatalf("fixture changed: method = %s", op.HTTP.Method)
	}
	if op.Effect != spec.EffectRead {
		t.Errorf("effect = %q, want read — a POST that searches must stay retryable", op.Effect)
	}
	if op.Effect.Mutating() {
		t.Error("a search must not read as mutating")
	}

	create, _ := pkg.Operation("probe.issue.create_issue")
	if create.Effect != spec.EffectCreate {
		t.Errorf("create effect = %q, want create", create.Effect)
	}
	if !create.Effect.Mutating() {
		t.Error("a create must read as mutating")
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

	pkg, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
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

func TestRejectsUnknownFormats(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no version marker", `{"paths": {}}`, "not an API description"},
		{"swagger 1.2", `{"swagger": "1.2", "paths": {}}`, "unsupported swagger version"},
		{"openapi 4", `{"openapi": "4.0.0", "paths": {}}`, "unsupported openapi version"},
		{"no path", `{"swagger": "2.0", "paths": {}}`, "declares no path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := gen.Generate([]byte(tc.body), gen.Options{ConnectorID: "probe"})
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
	pkg, err := gen.Generate([]byte(y), gen.Options{ConnectorID: "probe"})
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
	a, err := gen.Generate([]byte(swagger2Fixture), gen.Options{ConnectorID: "probe", Maturity: fixed})
	if err != nil {
		t.Fatal(err)
	}
	b, err := gen.Generate([]byte(swagger2Fixture), gen.Options{ConnectorID: "probe", Maturity: fixed})
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
