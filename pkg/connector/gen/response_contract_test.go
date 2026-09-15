package gen_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// oas3Items describes ONE operation whose 200 is a component reference. The
// component is deliberately awkward: an integer, a write-only secret that the
// vendor also lists as required, and a self-reference.
const oas3Items = `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "paths": {
    "/items/{id}": {
      "get": {
        "tags": ["item"], "operationId": "itemGet",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}
      }
    }
  },
  "components": {"schemas": {"Item": {
    "type": "object",
    "required": ["id", "secret"],
    "properties": {
      "id": {"type": "integer"},
      "secret": {"type": "string", "writeOnly": true},
      "label": {"type": "string", "nullable": true},
      "next": {"$ref": "#/components/schemas/Item"}
    }
  }}}
}`

// swagger2Items is the same API in Swagger 2.0, where `integer` is a LEXICAL
// property of the token rather than a mathematical one.
const swagger2Items = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "basePath": "/api/v1",
  "paths": {
    "/items/{id}": {
      "get": {
        "tags": ["item"], "operationId": "itemGet",
        "parameters": [{"name": "id", "in": "path", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/Item"}}}
      }
    }
  },
  "definitions": {"Item": {
    "type": "object",
    "required": ["id"],
    "properties": {"id": {"type": "integer"}, "label": {"type": "string", "x-nullable": true}}
  }}
}`

func generateWith(t *testing.T, body string, contracts bool) (*spec.Package, *gen.Report) {
	t.Helper()
	pkg, report, err := gen.Generate([]byte(body), gen.Options{
		ConnectorID:       "probe",
		ValidateResponses: contracts,
		Now:               func() time.Time { return time.Unix(0, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("generate (contracts=%v): %v", contracts, err)
	}
	return pkg, report
}

func onlyOp(t *testing.T, pkg *spec.Package) spec.Operation {
	t.Helper()
	ops := pkg.Operations()
	if len(ops) != 1 {
		t.Fatalf("fixture produced %d operations, want exactly one", len(ops))
	}
	return ops[0]
}

// TestGenerationWithoutTheOptionStaysV1 is the compatibility floor: every
// package generated before contracts existed must keep generating identically.
// A format that quietly became v2 would refuse to load on every reader in the
// field.
func TestGenerationWithoutTheOptionStaysV1(t *testing.T) {
	pkg, report := generateWith(t, oas3Items, false)

	if pkg.Connector.SchemaVersion != spec.LegacySchemaVersion {
		t.Errorf("schema_version = %d, want %d", pkg.Connector.SchemaVersion, spec.LegacySchemaVersion)
	}
	if pkg.ResponseSchemas != nil {
		t.Errorf("ResponseSchemas = %v, want nil when nobody asked", pkg.ResponseSchemas)
	}
	if len(report.Uncontracted) != 0 {
		t.Errorf("Uncontracted = %+v, want empty when nobody asked", report.Uncontracted)
	}
	for _, result := range onlyOp(t, pkg).Results {
		if result.ResponseSchemaRef != "" {
			t.Errorf("status %d carries %q, want no contract reference", result.Status, result.ResponseSchemaRef)
		}
	}

	dir := t.TempDir()
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.ResponsesFile)); !os.IsNotExist(err) {
		t.Errorf("%s exists (err=%v), want a v1 package to write none", spec.ResponsesFile, err)
	}
	// No new key may appear anywhere in the bytes a reviewer diffs.
	walkFiles(t, dir, func(path string, body []byte) {
		for _, forbidden := range []string{"response_schema_ref", "schema_version: 2"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s contains %q — the v1 output is not byte-identical any more", path, forbidden)
			}
		}
	})
}

func walkFiles(t *testing.T, dir string, fn func(path string, body []byte)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			walkFiles(t, full, fn)
			continue
		}
		body, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		fn(full, body)
	}
}

// TestAReferencedResponseBorrowsTheComponentContract pins the emitted shape:
// one contract per component rather than one per operation-and-status, the
// write-only field dropped from required, and the self-reference kept.
func TestAReferencedResponseBorrowsTheComponentContract(t *testing.T) {
	pkg, report := generateWith(t, oas3Items, true)
	if len(report.Uncontracted) != 0 {
		t.Fatalf("nothing should be unrepresentable here: %+v", report.Uncontracted)
	}
	if pkg.Connector.SchemaVersion != spec.ResponseContractsVersion {
		t.Errorf("schema_version = %d, want %d", pkg.Connector.SchemaVersion, spec.ResponseContractsVersion)
	}

	op := onlyOp(t, pkg)
	if got := op.Results[0].ResponseSchemaRef; got != "Item" {
		t.Fatalf("200 references %q, want the component name", got)
	}
	item, ok := pkg.ResponseSchemas["Item"]
	if !ok {
		t.Fatalf("contracts = %v, want one named Item", pkg.ResponseSchemas)
	}
	if len(item.Required) != 1 || item.Required[0] != "id" {
		t.Errorf("required = %v, want only id — `secret` is writeOnly, so the vendor never sends it in a response", item.Required)
	}
	if item.Properties["next"].Ref != "Item" {
		t.Errorf("next = %+v, want a self-reference the vocabulary carries natively", item.Properties["next"])
	}
	if !item.Properties["label"].Nullable {
		t.Error("label is declared nullable; a contract that forgets it REFUSES a null the vendor documented")
	}
	if item.Properties["id"].IntegerMode != "" {
		t.Errorf("OAS3 integer_mode = %q, want the mathematical default (OAS 3.0.4 settled 1.0 IS an integer)", item.Properties["id"].IntegerMode)
	}
	// The generated package must satisfy the reader that will load it.
	if err := pkg.ValidateResponseContracts(); err != nil {
		t.Fatalf("the generator emitted contracts its own reader refuses: %v", err)
	}
}

// TestTheIntegerDialectFollowsTheFormat is the one place the two descriptions
// legitimately DISAGREE, and both readings are correct for their dialect.
func TestTheIntegerDialectFollowsTheFormat(t *testing.T) {
	oas3, _ := generateWith(t, oas3Items, true)
	sw2, _ := generateWith(t, swagger2Items, true)

	if mode := oas3.ResponseSchemas["Item"].Properties["id"].IntegerMode; mode != "" {
		t.Errorf("OAS3 integer_mode = %q, want mathematical (https://spec.openapis.org/oas/v3.0.4.html#data-types)", mode)
	}
	if mode := sw2.ResponseSchemas["Item"].Properties["id"].IntegerMode; mode != "token" {
		t.Errorf("Swagger2 integer_mode = %q, want token — draft-04 defines integer on the TOKEN, so 1.0 is not one", mode)
	}
	// x-nullable is Swagger 2's spelling. Ignoring it would make the contract
	// refuse a null the vendor documented.
	if !sw2.ResponseSchemas["Item"].Properties["label"].Nullable {
		t.Error("x-nullable was ignored, so the contract now refuses a documented null")
	}
}

// TestAnExactNumberSurvivesGeneration runs numbers through the YAML path, where
// the legacy tree is float64 and the exact re-read is the whole point.
func TestAnExactNumberSurvivesGeneration(t *testing.T) {
	yamlDoc := func(enum string) string {
		return `
openapi: 3.0.0
info: {title: Probe, version: "1.0"}
paths:
  /items:
    get:
      tags: [item]
      operationId: itemList
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                properties:
                  id:
                    type: integer
                    enum: [` + enum + `]
`
	}

	t.Run("a member the run cannot deliver is reported, not emitted", func(t *testing.T) {
		pkg, report := generateWith(t, yamlDoc("9007199254740993"), true)
		if ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef; ref != "" {
			t.Errorf("ResponseSchemaRef = %q — a contract naming a value no run hands on must not ship", ref)
		}
		if len(report.Uncontracted) != 1 {
			t.Fatalf("Uncontracted = %+v, want exactly the operation whose enum cannot be delivered", report.Uncontracted)
		}
		if !strings.Contains(report.Uncontracted[0].Reason, "9007199254740993") {
			t.Errorf("Reason = %q, want the offending value named", report.Uncontracted[0].Reason)
		}
	})

	t.Run("a deliverable member keeps its SOURCE text", func(t *testing.T) {
		// The legacy float64 tree would hand back 1000; the exact re-read keeps
		// what the vendor wrote, and the contract compares by value regardless.
		pkg, report := generateWith(t, yamlDoc("1.00e3"), true)
		if len(report.Uncontracted) != 0 {
			t.Fatalf("unexpected refusal: %+v", report.Uncontracted)
		}
		ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef
		if ref == "" {
			t.Fatal("the inline response got no contract")
		}
		enum := pkg.ResponseSchemas[ref].Properties["id"].Enum
		if len(enum) != 1 || string(enum[0]) != "1.00e3" {
			t.Fatalf("enum = %v, want the source text 1.00e3", enum)
		}
		op := onlyOp(t, pkg)
		for _, equal := range []string{`{"id":1000}`, `{"id":1.0e3}`, `{"id":1000.0}`} {
			if _, err := pkg.ValidateResponse(op, 200, []byte(equal)); err != nil {
				t.Errorf("%s is the same number as 1.00e3 and was refused: %v", equal, err)
			}
		}
		if _, err := pkg.ValidateResponse(op, 200, []byte(`{"id":1001}`)); err == nil {
			t.Error("the generated contract stopped discriminating")
		}
	})
}

// TestAnUnrepresentableShapeGetsNoContractAndSaysWhy is the honesty rule.
//
// A contract emitted for a shape the vocabulary only half understands is worse
// than none: the operator reads "validated" and gets a check that passes on
// anything. So the response keeps its historical reading and the reason is
// reported against the operation and status.
func TestAnUnrepresentableShapeGetsNoContractAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		reason string
	}{
		{"composition", `{"allOf": [{"type": "object"}]}`, "allOf"},
		{"union", `{"oneOf": [{"type": "object"}, {"type": "string"}]}`, "oneOf"},
		{"a bound nobody models", `{"type": "integer", "minimum": 3}`, "minimum"},
		{"a closed object", `{"type": "object", "additionalProperties": false}`, "additionalProperties"},
		{"a 3.1 type union", `{"type": ["string", "null"]}`, "union of types"},
		{"tuple items", `{"type": "array", "items": [{"type": "string"}]}`, "tuple"},
		// Nested one level down: a bad reference in the RESPONSE position makes
		// the legacy walker drop the whole operation long before contracts are
		// considered, so the case has to sit where only this pass sees it.
		{"a remote reference", `{"type": "object", "properties": {"child": {"$ref": "https://example.invalid/Item.json"}}}`, "remote reference"},
		{"a dangling reference", `{"type": "object", "properties": {"child": {"$ref": "#/components/schemas/Absent"}}}`, "does not define"},
		{"a non-scalar enum member", `{"enum": [{"deep": true}]}`, "non-scalar enum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "paths": {"/items": {"get": {"tags": ["item"], "operationId": "itemList",
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": ` + tc.schema + `}}}}}}}
}`
			pkg, report := generateWith(t, body, true)
			if ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef; ref != "" {
				t.Fatalf("a shape the vocabulary cannot carry got contract %q", ref)
			}
			if len(report.Uncontracted) != 1 {
				t.Fatalf("Uncontracted = %+v, want exactly one reported limitation", report.Uncontracted)
			}
			got := report.Uncontracted[0]
			if got.Status != 200 || got.OperationID == "" {
				t.Errorf("the limitation must be locatable: %+v", got)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to name %q", got.Reason, tc.reason)
			}
		})
	}
}

// TestContractsSurviveTheRoundTripToDisk closes the loop: what the generator
// wrote is what a reader loads back.
func TestContractsSurviveTheRoundTripToDisk(t *testing.T) {
	pkg, _ := generateWith(t, oas3Items, true)
	dir := t.TempDir()
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, spec.ResponsesFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc struct {
		SchemaVersion int                            `json:"schema_version"`
		Connector     string                         `json:"connector"`
		Schemas       map[string]spec.ResponseSchema `json:"schemas"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", spec.ResponsesFile, err)
	}
	if doc.SchemaVersion != spec.ResponseContractsVersion || doc.Connector != "probe" {
		t.Errorf("document header = %d/%q", doc.SchemaVersion, doc.Connector)
	}
	back, err := spec.LoadGenerated(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(back.ResponseSchemas) != len(pkg.ResponseSchemas) {
		t.Fatalf("read back %d contracts, wrote %d", len(back.ResponseSchemas), len(pkg.ResponseSchemas))
	}
	op := onlyOp(t, back)
	if _, err := back.ValidateResponse(op, 200, []byte(`{"id":1}`)); err != nil {
		t.Errorf("a body the contract allows was refused after the round trip: %v", err)
	}
	if _, err := back.ValidateResponse(op, 200, []byte(`{"id":"one"}`)); err == nil {
		t.Error("the contract stopped discriminating after the round trip")
	}
}

// oneOp wraps a response schema into a description with exactly one operation.
func oneOp(schema, components string) string {
	return `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "paths": {"/items": {"get": {"tags": ["item"], "operationId": "itemList",
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": ` + schema + `}}}}}}}` +
		components + `
}`
}

// TestAFailedComponentTakesItsHalfBuiltSiblingsWithIt.
//
// A component that failed on an unrepresentable keyword still left behind the
// components it had already built. When one of those referenced the failed one
// — A → B → A, which is what `Issue.user` / `User.issues` is — the map held a
// DANGLING reference, ValidateGenerated refused the whole package, and
// `connectors gen --validate-responses` exited 1 with a message blaming the
// generator. Whether it happened depended on the ALPHABETICAL order of the
// properties: the failing one sorting after the referencing one was the
// difference between a clean report and a dead command. (Revi/rva finding 1.)
func TestAFailedComponentTakesItsHalfBuiltSiblingsWithIt(t *testing.T) {
	// `zz` sorts AFTER `b`, so B is built before A fails — the ordering that
	// used to strand the reference.
	body := oneOp(`{"$ref": "#/components/schemas/A"}`, `,
  "components": {"schemas": {
    "A": {"type": "object", "properties": {"b": {"$ref": "#/components/schemas/B"}, "zz": {"type": "string", "minLength": 1}}},
    "B": {"type": "object", "properties": {"a": {"$ref": "#/components/schemas/A"}}}
  }}`)

	pkg, report := generateWith(t, body, true)
	if len(report.Uncontracted) != 1 || !strings.Contains(report.Uncontracted[0].Reason, "minLength") {
		t.Fatalf("Uncontracted = %+v, want one limitation naming minLength", report.Uncontracted)
	}
	if ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef; ref != "" {
		t.Errorf("the response got contract %q from a build that failed", ref)
	}
	if len(pkg.ResponseSchemas) != 0 {
		t.Errorf("contracts = %v, want none: a failed build must leave the map as it found it", pkg.ResponseSchemas)
	}
	// The package the generator hands back must satisfy its own reader.
	if err := pkg.ValidateResponseContracts(); err != nil {
		t.Fatalf("the generator emitted a package its own reader refuses: %v", err)
	}
}

// TestTwoSchemasCannotShareOneContractName.
//
// Contracts were keyed by the LAST SEGMENT of the $ref, and `component()`
// returned an already-built name without ever comparing the resolved target.
// A description carrying both #/definitions/Thing and #/components/schemas/Thing
// — an ordinary residue of a Swagger 2 → OAS 3 conversion — merged two
// different shapes under one contract, with no warning, and refused at runtime
// the body the vendor documents for whichever operation lost.
func TestTwoSchemasCannotShareOneContractName(t *testing.T) {
	body := `{
  "openapi": "3.0.0",
  "info": {"title": "Probe", "version": "1.0"},
  "paths": {
    "/a": {"get": {"tags": ["item"], "operationId": "itemGetA",
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}}},
    "/b": {"get": {"tags": ["item"], "operationId": "itemGetB",
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/definitions/Thing"}}}}}}}
  },
  "components": {"schemas": {"Thing": {"type": "object", "required": ["modern_only"], "properties": {"modern_only": {"type": "string"}}}}},
  "definitions": {"Thing": {"type": "object", "required": ["legacy_only"], "properties": {"legacy_only": {"type": "string"}}}}
}`
	pkg, report := generateWith(t, body, true)

	var collided bool
	for _, u := range report.Uncontracted {
		if strings.Contains(u.Reason, "share the contract name") {
			collided = true
		}
	}
	if !collided {
		t.Fatalf("Uncontracted = %+v, want the name collision reported rather than merged", report.Uncontracted)
	}
	// Whatever survived must describe ONE shape: no operation may reference a
	// contract built from a different schema than its own.
	for _, op := range pkg.Operations() {
		for _, res := range op.Results {
			if res.ResponseSchemaRef == "" {
				continue
			}
			c := pkg.ResponseSchemas[res.ResponseSchemaRef]
			want := "modern_only"
			if strings.HasSuffix(op.ID, "get_b") {
				want = "legacy_only"
			}
			if len(c.Required) != 1 || c.Required[0] != want {
				t.Errorf("%s references contract %q requiring %v, want %q — two shapes merged", op.ID, res.ResponseSchemaRef, c.Required, want)
			}
		}
	}
}

// TestWriteOnlyIsHonouredThroughAReference: a `password` property is routinely
// a reference to a component that carries the writeOnly, and testing the
// property node alone saw nothing — so `password` went into required, and the
// vendor, which by definition never sends it back, failed EVERY call.
func TestWriteOnlyIsHonouredThroughAReference(t *testing.T) {
	body := oneOp(`{"$ref": "#/components/schemas/User"}`, `,
  "components": {"schemas": {
    "User": {"type": "object", "required": ["id", "password"], "properties": {"id": {"type": "string"}, "password": {"$ref": "#/components/schemas/Password"}}},
    "Password": {"type": "string", "writeOnly": true}
  }}`)

	pkg, _ := generateWith(t, body, true)
	user, ok := pkg.ResponseSchemas["User"]
	if !ok {
		t.Fatalf("contracts = %v, want one named User", pkg.ResponseSchemas)
	}
	if len(user.Required) != 1 || user.Required[0] != "id" {
		t.Fatalf("required = %v, want only id — the vendor never sends a writeOnly field back", user.Required)
	}
	op := onlyOp(t, pkg)
	if _, err := pkg.ValidateResponse(op, 200, []byte(`{"id":"u1"}`)); err != nil {
		t.Errorf("the body the vendor actually sends was refused: %v", err)
	}
}

// TestANullableEnumListsNull: the reader applies an enum to a null like any
// other value — deliberately, so `enum: ["ok"]` cannot silently accept null.
// The generator therefore has to say null is permitted in the only place the
// reader looks. `{type: string, enum: [...], nullable: true}` is the commonest
// shape in the wild; without this every documented null was refused.
func TestANullableEnumListsNull(t *testing.T) {
	body := oneOp(`{"type": "object", "properties": {"state": {"type": "string", "enum": ["open", "closed"], "nullable": true}}}`, ``)
	pkg, _ := generateWith(t, body, true)
	op := onlyOp(t, pkg)

	for _, tc := range []struct {
		body   string
		accept bool
	}{
		{`{"state":null}`, true},
		{`{"state":"open"}`, true},
		{`{"state":"other"}`, false},
	} {
		_, err := pkg.ValidateResponse(op, 200, []byte(tc.body))
		if accepted := err == nil; accepted != tc.accept {
			t.Errorf("%s accepted=%v, want %v (err=%v)", tc.body, accepted, tc.accept, err)
		}
	}
}

// TestSpecExtensionsDoNotStopAContract.
//
// An `x-` key is non-normative BY DEFINITION in the OpenAPI specification, so
// ignoring it is one rule read off the spec rather than one more spelling on a
// list. Measured on the repository's reference description: 335 of 339 refused
// responses were refused for `x-go-package` alone, a go-swagger metadata tag
// that constrains nothing.
func TestSpecExtensionsDoNotStopAContract(t *testing.T) {
	body := oneOp(`{"type": "object", "x-go-package": "code.gitea.io/gitea/modules/structs", "properties": {"id": {"type": "integer", "x-go-name": "ID"}}}`, ``)
	pkg, report := generateWith(t, body, true)
	if len(report.Uncontracted) != 0 {
		t.Fatalf("Uncontracted = %+v, want none: a spec extension carries no validation semantics", report.Uncontracted)
	}
	ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef
	if ref == "" {
		t.Fatal("a schema whose only unknown keys are spec extensions got no contract")
	}
	// And the contract still discriminates — ignoring x- must not mean ignoring
	// the schema.
	op := onlyOp(t, pkg)
	if _, err := pkg.ValidateResponse(op, 200, []byte(`{"id":"not-an-integer"}`)); err == nil {
		t.Error("the contract stopped checking anything")
	}
}

// TestSwagger2HonoursProduces.
//
// Swagger 2 anchors the media type on the OPERATION, not on the response, so
// `schema: {type: string}` under `produces: [text/plain]` describes a RAW body.
// Reading the schema without the media type put contracts on text/plain,
// text/html and an application/zip — eleven of them on the repository's
// reference description — each refusing every answer the endpoint can give.
func TestSwagger2HonoursProduces(t *testing.T) {
	sw2 := func(produces string) string {
		return `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "paths": {"/token": {"get": {"tags": ["item"], "operationId": "itemToken", ` + produces + `
    "responses": {"200": {"description": "ok", "schema": {"type": "string"}}}}}}
}`
	}

	t.Run("a raw body gets no contract, and says why", func(t *testing.T) {
		pkg, report := generateWith(t, sw2(`"produces": ["text/plain"],`), true)
		if ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef; ref != "" {
			t.Errorf("a text/plain body got contract %q", ref)
		}
		if len(report.Uncontracted) != 1 || !strings.Contains(report.Uncontracted[0].Reason, "does not produce JSON") {
			t.Fatalf("Uncontracted = %+v, want the media type named", report.Uncontracted)
		}
	})

	t.Run("a JSON body still gets one", func(t *testing.T) {
		pkg, _ := generateWith(t, sw2(`"produces": ["application/json"],`), true)
		if onlyOp(t, pkg).Results[0].ResponseSchemaRef == "" {
			t.Error("a JSON operation lost its contract")
		}
	})

	t.Run("a structured JSON suffix counts as JSON", func(t *testing.T) {
		pkg, _ := generateWith(t, sw2(`"produces": ["application/vnd.api+json"],`), true)
		if onlyOp(t, pkg).Results[0].ResponseSchemaRef == "" {
			t.Error("application/vnd.api+json is a JSON body; refusing it drops a legitimate contract")
		}
	})

	t.Run("no produces at all leaves the historical behaviour", func(t *testing.T) {
		pkg, _ := generateWith(t, sw2(``), true)
		if onlyOp(t, pkg).Results[0].ResponseSchemaRef == "" {
			t.Error("a description that declares no media type must not lose its contract")
		}
	})
}

// TestABodyOnlyOneDecoderSeesIsReportedRatherThanDropped.
//
// Operations are built from a generic decode; contracts are derived from a
// second, exact re-read by a DIFFERENT library. Where the two disagree, the
// exact pass finds no body and the code that means "the vendor declared none"
// runs — so the response silently leaves with no contract AND no line in the
// report, which is the one thing an operator who asked for validation reads.
//
// A YAML merge key is the instance that exposed it: the generic pass expands
// `<<:`, the exact walk keeps a literal "<<" member. The guard does not look
// for merge keys — it compares the two trees, so any future divergence between
// the decoders surfaces the same way.
func TestABodyOnlyOneDecoderSeesIsReportedRatherThanDropped(t *testing.T) {
	const merged = `
openapi: "3.0.0"
info: {title: Probe, version: "1.0"}
x-shared:
  ok: &ok
    description: ok
    content:
      application/json:
        schema:
          type: object
          required: [id]
          properties: {id: {type: integer}}
paths:
  /a:
    get:
      tags: [item]
      operationId: itemMerged
      responses:
        "200":
          <<: *ok
  /b:
    get:
      tags: [item]
      operationId: itemAliased
      responses:
        "200": *ok
`
	pkg, report := generateWith(t, merged, true)

	byID := map[string]spec.Operation{}
	for _, op := range pkg.Operations() {
		byID[op.ID] = op
	}
	aliased, ok := byID["probe.item.aliased"]
	if !ok {
		t.Fatalf("fixture produced %v, want probe.item.aliased among them", byID)
	}
	if aliased.Results[0].ResponseSchemaRef == "" {
		t.Error("a plain alias reads identically under both decoders and must keep its contract")
	}

	merged200 := byID["probe.item.merged"]
	if len(merged200.Results) == 0 {
		t.Fatal("fixture lost the merged operation entirely")
	}
	if ref := merged200.Results[0].ResponseSchemaRef; ref != "" {
		t.Errorf("ResponseSchemaRef = %q — the exact pass cannot have built a contract it never saw", ref)
	}
	var reported *gen.Uncontracted
	for i := range report.Uncontracted {
		if report.Uncontracted[i].OperationID == "probe.item.merged" {
			reported = &report.Uncontracted[i]
		}
	}
	if reported == nil {
		t.Fatalf("Uncontracted = %+v, want the operation whose body vanished between the two decoders", report.Uncontracted)
	}
	if !strings.Contains(reported.Reason, "exact re-read") {
		t.Errorf("Reason = %q, want it to name the disagreement rather than blame the vendor", reported.Reason)
	}
}
