package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

func responsePackage(schema ResponseSchema) (*Package, Operation) {
	op := Operation{ID: "probe.items.get", HTTP: HTTPBinding{Method: "GET", Path: "/items"}, Results: []ResultCase{{Status: 200, ResponseSchemaRef: "result"}}}
	p := &Package{
		Connector:       Connector{SchemaVersion: ResponseContractsVersion, ID: "probe"},
		Ops:             []OpsFile{{SchemaVersion: ResponseContractsVersion, Connector: "probe", Operations: []Operation{op}}},
		ResponseSchemas: map[string]ResponseSchema{"result": schema},
	}
	return p, op
}

func rawEnum(values ...string) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for i, value := range values {
		result[i] = json.RawMessage(value)
	}
	return result
}

func TestResponseContractsPreserveExactScalarValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		schema  ResponseSchema
		valid   []string
		invalid []string
	}{
		{"large integer", ResponseSchema{Type: "integer", Enum: rawEnum("9007199254740993")}, []string{"9007199254740993", "9007199254740993.0", "90071992547409930e-1"}, []string{"9007199254740992", `"9007199254740993"`, "true", "null"}},
		{"integer values", ResponseSchema{Type: "integer"}, []string{"1", "1.0", "1e3", "10e-1", "-0.0", "1e1000000"}, []string{"1.1", "1e-3", `"1"`, "false", "null"}},
		{"integer tokens", ResponseSchema{Type: "integer", IntegerMode: "token"}, []string{"1", "-1", "9007199254740993"}, []string{"1.0", "1e3"}},
		{"numeric enum", ResponseSchema{Enum: rawEnum("1.00e3", "false")}, []string{"1000", "1000.0", "false"}, []string{`"1000"`, `"false"`, "true", "0"}},
		{"string enum", ResponseSchema{Enum: rawEnum(`"true"`, `"1"`, `"null"`)}, []string{`"true"`, `"1"`, `"null"`}, []string{"true", "1", "null"}},
		{"nullable with enum", ResponseSchema{Type: "string", Nullable: true, Enum: rawEnum(`"ok"`, "null")}, []string{`"ok"`, "null"}, []string{`"other"`, "1"}},
		{"nullable does not override enum", ResponseSchema{Type: "string", Nullable: true, Enum: rawEnum(`"ok"`)}, []string{`"ok"`}, []string{"null"}},
		{"enum does not override type", ResponseSchema{Type: "string", Enum: rawEnum(`"ok"`, "null")}, []string{`"ok"`}, []string{"null"}},
		{"properties imply no type", ResponseSchema{Properties: map[string]ResponseSchema{"id": {Type: "integer"}}, Required: []string{"id"}}, []string{`"not an object"`, "null", "1", `{"id":1,"extra":true}`}, []string{`{}`, `{"id":"1"}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, op := responsePackage(test.schema)
			if err := p.ValidateResponseContracts(); err != nil {
				t.Fatal(err)
			}
			for _, body := range test.valid {
				checked, err := p.ValidateResponse(op, 200, []byte(body))
				if err != nil || !checked {
					t.Errorf("valid %s: checked=%v error=%v", body, checked, err)
				}
			}
			for _, body := range test.invalid {
				if checked, err := p.ValidateResponse(op, 200, []byte(body)); !checked || err == nil {
					t.Errorf("invalid %s accepted", body)
				}
			}
		})
	}
}

func TestResponseContractIsExplicitAndStatusSpecific(t *testing.T) {
	p, op := responsePackage(ResponseSchema{Type: "null"})
	if checked, err := p.ValidateResponse(op, 201, []byte(`"other shape"`)); checked || err != nil {
		t.Fatal(checked, err)
	}
	if _, err := p.ValidateResponse(op, 200, nil); err == nil {
		t.Fatal("empty body was confused with null")
	}
	if checked, err := p.ValidateResponse(op, 200, []byte("null")); !checked || err != nil {
		t.Fatal(checked, err)
	}
	legacy := Operation{Results: []ResultCase{{Status: 204, SchemaRef: "object-description"}}}
	if checked, err := p.ValidateResponse(legacy, 204, nil); checked || err != nil {
		t.Fatal("legacy description was enforced", err)
	}
	for _, version := range []int{0, LegacySchemaVersion, SchemaVersion + 1} {
		p.Connector.SchemaVersion = version
		if err := p.ValidateResponseContracts(); err == nil {
			t.Errorf("version %d accepted a v2 contract", version)
		}
	}
}

func TestResponseContractReferencesAreCheckedBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		schemas map[string]ResponseSchema
		valid   bool
	}{
		{"missing", map[string]ResponseSchema{"result": {Ref: "absent"}}, false},
		{"self alias", map[string]ResponseSchema{"result": {Ref: "result"}}, false},
		{"mutual alias", map[string]ResponseSchema{"result": {Ref: "other"}, "other": {Ref: "result"}}, false},
		{"recursive object", map[string]ResponseSchema{"result": {Type: "object", Properties: map[string]ResponseSchema{"next": {Ref: "result"}}}}, true},
		{"recursive array", map[string]ResponseSchema{"result": {Type: "array", Items: &ResponseSchema{Ref: "result"}}}, true},
		{"ref siblings", map[string]ResponseSchema{"result": {Ref: "other", Nullable: true}, "other": {Type: "object"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _ := responsePackage(ResponseSchema{})
			p.ResponseSchemas = test.schemas
			if err := p.ValidateResponseContracts(); (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
	p, op := responsePackage(ResponseSchema{Type: "object", Properties: map[string]ResponseSchema{"next": {Ref: "result"}}})
	if _, err := p.ValidateResponse(op, 200, []byte(`{"next":{"next":{}}}`)); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat(`{"next":`, 100) + `{}` + strings.Repeat(`}`, 100)
	if _, err := p.ValidateResponse(op, 200, []byte(body)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("unbounded recursive response: %v", err)
	}
	other := Operation{ID: "probe.other", HTTP: HTTPBinding{Method: "GET"}, Results: []ResultCase{{Status: 200, ResponseSchemaRef: "missing"}}}
	if err := p.ValidateResponseContracts(other); err == nil {
		t.Fatal("standalone operation's missing ref passed preflight")
	}
}

func TestResponseContractDiagnosticsExcludeResponseValues(t *testing.T) {
	p, op := responsePackage(ResponseSchema{Type: "object", Required: []string{"count"}, Properties: map[string]ResponseSchema{"count": {Type: "integer"}}})
	secret := "vendor-private-token-DO-NOT-PUBLISH"
	for _, body := range []string{`{"count":"` + secret + `"}`, `{"count":1,"secret":"` + secret + `"} garbage`, secret} {
		_, err := p.ValidateResponse(op, 200, []byte(body))
		if err == nil {
			t.Fatal("invalid response accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("response leaked: %v", err)
		}
	}
}

func TestResponseContractBoundsAndClosedVocabulary(t *testing.T) {
	for _, s := range []ResponseSchema{
		{Type: "made-up"}, {Nullable: true}, {Type: "number", IntegerMode: "token"},
		{Enum: []json.RawMessage{}}, {Enum: rawEnum(`{"not":"scalar"}`)},
		{Enum: rawEnum(`1e99999999999999999999999999999`)},
		{Required: []string{"id", "id"}},
	} {
		p, _ := responsePackage(s)
		if err := p.ValidateResponseContracts(); err == nil {
			t.Errorf("invalid contract accepted: %+v", s)
		}
	}
	p, op := responsePackage(ResponseSchema{Type: "array", Items: &ResponseSchema{Type: "integer", Enum: rawEnum("1", "2", "3", "4", "5", "6", "7", "8", "9", "10")}})
	body := `[` + strings.Repeat(`10,`, 10000) + `10]`
	if _, err := p.ValidateResponse(op, 200, []byte(body)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("enum comparisons are unbounded: %v", err)
	}
}
