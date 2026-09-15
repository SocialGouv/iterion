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

// TestAnExactNumberSurvivesGeneration runs the collision through the YAML path,
// where the legacy tree is float64.
func TestAnExactNumberSurvivesGeneration(t *testing.T) {
	const yamlDoc = `
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
                    enum: [9007199254740993]
`
	pkg, report := generateWith(t, yamlDoc, true)
	if len(report.Uncontracted) != 0 {
		t.Fatalf("unexpected refusal: %+v", report.Uncontracted)
	}
	ref := onlyOp(t, pkg).Results[0].ResponseSchemaRef
	if ref == "" {
		t.Fatal("the inline response got no contract")
	}
	enum := pkg.ResponseSchemas[ref].Properties["id"].Enum
	if len(enum) != 1 {
		t.Fatalf("enum = %v, want one member", enum)
	}
	if got := string(enum[0]); got != "9007199254740993" {
		t.Fatalf("enum member = %s, want the SOURCE text — float64 rounds it to ...992", got)
	}
	// And the contract the generator wrote actually discriminates.
	op := onlyOp(t, pkg)
	if _, err := pkg.ValidateResponse(op, 200, []byte(`{"id":9007199254740992}`)); err == nil {
		t.Error("the generated contract accepted the float64 collision")
	}
	if _, err := pkg.ValidateResponse(op, 200, []byte(`{"id":9007199254740993}`)); err != nil {
		t.Errorf("the generated contract refused its own enum member: %v", err)
	}
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
