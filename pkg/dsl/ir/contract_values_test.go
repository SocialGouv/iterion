package ir

import (
	"encoding/json"
	"math"
	"testing"
)

func TestPublicValuesPreserveMissingNullAndEmpty(t *testing.T) {
	array, _ := ResolvePortType("string[]", nil)
	text, _ := ResolvePortType("string", nil)
	ports := []PublicPort{
		{Name: "items", Type: array, Required: true},
		{Name: "note", Type: text, Required: false, Nullable: true},
	}
	for _, input := range []map[string]any{{}, {"items": nil}, {"items": []string(nil)}} {
		if err := ValidatePublicValues(ports, input); err == nil {
			t.Fatalf("missing/null array was accepted: %#v", input)
		}
	}
	for _, input := range []map[string]any{{"items": []string{}}, {"items": []any{}, "note": nil}} {
		if err := ValidatePublicValues(ports, input); err != nil {
			t.Fatal(err)
		}
	}
	min := 1
	ports[0].MinItems = &min
	if err := ValidatePublicValues(ports, map[string]any{"items": []any{}}); err == nil {
		t.Fatal("an empty collection satisfied min_items: 1")
	}
}

func TestPublicValuesRejectCoercionAndPreserveLargeIntegers(t *testing.T) {
	typ, _ := ResolvePortType("int", nil)
	for _, value := range []any{"1", 1.2, true, math.NaN(), math.Inf(1), float64(1 << 54), json.Number("1/2")} {
		if err := typ.ValidateValue(value); err == nil {
			t.Fatalf("accepted invalid or imprecise integer %#v", value)
		}
	}
	value, err := DecodePortValue([]byte("9007199254740993"))
	if err != nil || value.(json.Number).String() != "9007199254740993" {
		t.Fatal("JSON integer precision was lost")
	}
	if err := typ.ValidateValue(value); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePortValue([]byte("1 2")); err == nil {
		t.Fatal("accepted trailing JSON value")
	}
}

func TestPublicCriteriaAreDeterministicAndTyped(t *testing.T) {
	contract := &PublicContract{Criteria: []PublicCriterion{
		{Name: "not_empty", Kind: "min_length", Port: "output.text", Params: json.RawMessage(`{"min":2}`)},
	}}
	if err := contract.ValidateCriteria("output", map[string]any{"text": "éé"}); err != nil {
		t.Fatal(err)
	}
	if err := contract.ValidateCriteria("output", map[string]any{"text": "é"}); err == nil {
		t.Fatal("criterion counted bytes instead of Unicode characters")
	}
	contract.Criteria[0].Params = json.RawMessage(`{"min":"2"}`)
	if err := contract.ValidateCriteria("output", map[string]any{"text": "valid"}); err == nil {
		t.Fatal("criterion parameter string was coerced to an integer")
	}
}
