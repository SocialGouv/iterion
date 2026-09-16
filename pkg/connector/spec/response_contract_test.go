package spec

import (
	"encoding/json"
	"strconv"
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
		// 9007199254740992 is the largest integer float64 holds exactly, and
		// ...993 shares its float64. A validator decoding through the DELIVERED
		// projection would accept either one for an enum naming the other; this
		// contract must not, which is what the exact decode buys. An enum
		// naming ...993 is refused outright — TestAContractCannotNameANumberNoRunDelivers.
		{"large integer", ResponseSchema{Type: "integer", Enum: rawEnum("9007199254740992")}, []string{"9007199254740992", "9007199254740992.0", "90071992547409920e-1"}, []string{"9007199254740993", `"9007199254740992"`, "true", "null"}},
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

// TestAContractCannotNameANumberNoRunDelivers.
//
// Validation reads the body exactly; Result.Data reaches the workflow through
// encoding/json's untyped float64. Past 2^53 the two disagree, so a contract
// naming 9007199254740993 would certify the value the vendor sent while the
// node hands on ...992 — the value that same contract refuses. The claim is
// false by construction, so it is refused where the package is admitted rather
// than discovered by a `.bot` deleting the wrong object.
//
// The refusal is on NAMING a value, not on carrying one: a type check stays
// true of both numbers and is deliberately untouched.
func TestAContractCannotNameANumberNoRunDelivers(t *testing.T) {
	for _, test := range []struct {
		name    string
		schema  ResponseSchema
		refused bool
	}{
		{"an enum past 2^53", ResponseSchema{Type: "integer", Enum: rawEnum("9007199254740993")}, true},
		{"the same value spelled with an exponent", ResponseSchema{Type: "integer", Enum: rawEnum("90071992547409930e-1")}, true},
		{"one undeliverable member among deliverable ones", ResponseSchema{Type: "integer", Enum: rawEnum("1", "9007199254740993")}, true},
		{"nested under a property", ResponseSchema{
			Type:       "object",
			Properties: map[string]ResponseSchema{"id": {Type: "integer", Enum: rawEnum("9007199254740993")}},
		}, true},
		// The other direction: everything float64 carries stays nameable.
		{"the largest exactly-held integer", ResponseSchema{Type: "integer", Enum: rawEnum("9007199254740992")}, false},
		{"an ordinary integer", ResponseSchema{Type: "integer", Enum: rawEnum("42")}, false},
		{"a fraction float64 holds", ResponseSchema{Enum: rawEnum("0.5")}, false},
		// Naming nothing claims nothing: the type check is untouched, and a
		// body past 2^53 still validates against it.
		{"a type check over the same range", ResponseSchema{Type: "integer"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, op := responsePackage(test.schema)
			err := p.ValidateResponseContracts()
			if refused := err != nil; refused != test.refused {
				t.Fatalf("refused = %v, want %v (err=%v)", refused, test.refused, err)
			}
			if test.refused {
				if !strings.Contains(err.Error(), "9007199254740993") {
					t.Errorf("the refusal must name the offending number: %v", err)
				}
				return
			}
			if test.schema.Type == "integer" && test.schema.Enum == nil {
				if checked, err := p.ValidateResponse(op, 200, []byte("9007199254740993")); !checked || err != nil {
					t.Errorf("a type check refused a body it describes correctly: checked=%v err=%v", checked, err)
				}
			}
		})
	}
}

// TestTheTraversalBudgetBoundsONEContractNotTheWholePackage.
//
// The budget exists to stop a pathological SHAPE — a contract that expands
// without end — and a counter shared by every top-level contract does not
// measure that. It measures how many contracts a package has, which the
// document size cap already bounds, and the two limits contradict: the package
// may hold 1024 contracts while the shared budget affords about ten nodes
// each. A description that is merely LARGE then fails to generate, blaming
// whichever contract sorted first.
func TestTheTraversalBudgetBoundsONEContractNotTheWholePackage(t *testing.T) {
	ordinary := func(properties int) ResponseSchema {
		s := ResponseSchema{Type: "object", Properties: map[string]ResponseSchema{}}
		for i := range properties {
			s.Properties["p"+strconv.Itoa(i)] = ResponseSchema{Type: "string"}
		}
		return s
	}

	t.Run("many ordinary contracts are admitted", func(t *testing.T) {
		// 500 × 26 nodes ≈ 13000: no single contract is anywhere near the
		// ceiling, and their SUM is over it.
		p, _ := responsePackage(ordinary(25))
		for i := range 500 {
			name := "c" + strconv.Itoa(i)
			p.ResponseSchemas[name] = ordinary(25)
			p.Ops[0].Operations[0].Results = append(p.Ops[0].Operations[0].Results,
				ResultCase{Status: 200, ResponseSchemaRef: name})
		}
		if err := p.ValidateResponseContracts(); err != nil {
			t.Fatalf("a package of ordinary contracts was refused for being numerous: %v", err)
		}
	})

	t.Run("one runaway contract is still refused", func(t *testing.T) {
		// The guard must keep biting where it means something: a single shape
		// whose own expansion passes the ceiling.
		runaway := ResponseSchema{Type: "object", Properties: map[string]ResponseSchema{}}
		for i := range 1000 {
			runaway.Properties["g"+strconv.Itoa(i)] = ordinary(20)
		}
		p, _ := responsePackage(runaway)
		err := p.ValidateResponseContracts()
		if err == nil {
			t.Fatal("a contract expanding past the ceiling must still be refused")
		}
		if !strings.Contains(err.Error(), "traversal limit") {
			t.Errorf("err = %v, want the traversal limit named", err)
		}
	})
}

// TestAnOrdinaryPageIsNotRefusedForBeingLarge.
//
// The transport already bounds a body at 32 MiB and decodes it whole before
// this walk begins, so a fixed node ceiling here does not protect memory — it
// only refuses pages the vendor was allowed to send. A 2000-row page of sixty
// fields is an ordinary list response, and it was refused at row 1639; on a
// mutation that is a parked run rather than a failure a workflow can branch on.
//
// The budget now comes from the body's own length, which a JSON tree cannot
// exceed in nodes. It still terminates a walk whose cost is NOT linear in the
// body — the enum scan, which pays per member per value.
func TestAnOrdinaryPageIsNotRefusedForBeingLarge(t *testing.T) {
	t.Run("a large ordinary page validates", func(t *testing.T) {
		row := ResponseSchema{Type: "object", Properties: map[string]ResponseSchema{}}
		for i := range 60 {
			row.Properties["f"+strconv.Itoa(i)] = ResponseSchema{Type: "string"}
		}
		p, op := responsePackage(ResponseSchema{Type: "array", Items: &row})
		if err := p.ValidateResponseContracts(); err != nil {
			t.Fatalf("preflight: %v", err)
		}
		var page strings.Builder
		page.WriteByte('[')
		for r := range 2000 {
			if r > 0 {
				page.WriteByte(',')
			}
			page.WriteByte('{')
			for i := range 60 {
				if i > 0 {
					page.WriteByte(',')
				}
				page.WriteString(`"f` + strconv.Itoa(i) + `":"v"`)
			}
			page.WriteByte('}')
		}
		page.WriteByte(']')
		checked, err := p.ValidateResponse(op, 200, []byte(page.String()))
		if !checked {
			t.Fatal("the contract was not applied at all")
		}
		if err != nil {
			t.Fatalf("an ordinary %d-byte page was refused: %v", page.Len(), err)
		}
	})

	t.Run("a page of values against one enum validates too", func(t *testing.T) {
		// Comparison is against a set decoded once per body, so a page does not
		// pay values × members. What the budget still bounds is the DECODING of
		// many distinct enums — proved in
		// TestResponseContractBoundsAndClosedVocabulary, which is where that
		// backstop lives rather than here.
		members := make([]json.RawMessage, 1024)
		for i := range members {
			members[i] = json.RawMessage(strconv.Itoa(i + 1000000))
		}
		p, op := responsePackage(ResponseSchema{Type: "array", Items: &ResponseSchema{Enum: members}})
		if err := p.ValidateResponseContracts(); err != nil {
			t.Fatalf("preflight: %v", err)
		}
		// The LAST member, so a per-value scan would pay the whole list.
		last := strconv.Itoa(1000000 + len(members) - 1)
		var body strings.Builder
		body.WriteByte('[')
		for i := range 5000 {
			if i > 0 {
				body.WriteByte(',')
			}
			body.WriteString(last)
		}
		body.WriteByte(']')
		if _, err := p.ValidateResponse(op, 200, []byte(body.String())); err != nil {
			t.Fatalf("five thousand values against one enum were refused: %v", err)
		}
	})
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
		// Nullability is the ONE sibling a reference may carry: it does not
		// qualify the referenced shape, it says the slot may hold none.
		{"nullable ref", map[string]ResponseSchema{"result": {Ref: "other", Nullable: true}, "other": {Type: "object"}}, true},
		{"ref with a type", map[string]ResponseSchema{"result": {Ref: "other", Type: "object"}, "other": {Type: "object"}}, false},
		{"ref with an enum", map[string]ResponseSchema{"result": {Ref: "other", Enum: []json.RawMessage{json.RawMessage(`"a"`)}}, "other": {Type: "string"}}, false},
		{"ref with properties", map[string]ResponseSchema{"result": {Ref: "other", Properties: map[string]ResponseSchema{"x": {Type: "string"}}}, "other": {Type: "object"}}, false},
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
	// A hundred levels is deep but not pathological, and the contract describes
	// it exactly. The value walk used to share the CONTRACT graph's ceiling and
	// refused this, for a body the pre-flight check had accepted.
	deep := strings.Repeat(`{"next":`, 100) + `{}` + strings.Repeat(`}`, 100)
	if _, err := p.ValidateResponse(op, 200, []byte(deep)); err != nil {
		t.Fatalf("a hundred conformant levels were refused: %v", err)
	}
	// The recursion is still bounded, one order of magnitude further out.
	body := strings.Repeat(`{"next":`, 1200) + `{}` + strings.Repeat(`}`, 1200)
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
	// Enum comparison no longer costs values × members: each enum is decoded
	// once per body and compared as a set. The bound is structural rather than
	// a counter, which is why a page of ten thousand values now validates
	// instead of tripping a limit on a shape that was never pathological.
	p, op := responsePackage(ResponseSchema{Type: "array", Items: &ResponseSchema{Type: "integer", Enum: rawEnum("1", "2", "3", "4", "5", "6", "7", "8", "9", "10")}})
	body := `[` + strings.Repeat(`10,`, 10000) + `10]`
	if _, err := p.ValidateResponse(op, 200, []byte(body)); err != nil {
		t.Fatalf("ten thousand values against a ten-member enum were refused: %v", err)
	}
	// What the budget still bounds is the DECODING of the enums themselves: a
	// small body reaching many distinct large enums pays per member, once each.
	many := ResponseSchema{Type: "object", Properties: map[string]ResponseSchema{}}
	members := make([]json.RawMessage, 1024)
	var wide strings.Builder
	wide.WriteByte('{')
	for i := range 400 {
		for m := range members {
			members[m] = json.RawMessage(strconv.Itoa(i*100000 + m))
		}
		name := "p" + strconv.Itoa(i)
		many.Properties[name] = ResponseSchema{Type: "integer", Enum: append([]json.RawMessage(nil), members...)}
		if i > 0 {
			wide.WriteByte(',')
		}
		wide.WriteString(`"` + name + `":` + strconv.Itoa(i*100000))
	}
	wide.WriteByte('}')
	pw, opw := responsePackage(many)
	if err := pw.ValidateResponseContracts(); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, err := pw.ValidateResponse(opw, 200, []byte(wide.String())); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("enum decoding is unbounded: %v", err)
	}
}
