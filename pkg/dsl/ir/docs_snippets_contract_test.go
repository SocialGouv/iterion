package ir

import (
	"reflect"
	"testing"
)

// A contract fragment on a documentation page is excused only for what it
// references elsewhere on the page — an input's var, an output's producer
// — never for a shape error of its own, which the same codes also carry.
func TestFragmentExcuseKeepsContractShapeErrors(t *testing.T) {
	errs := []string{
		`error [C300]: contract "c": input "goal" is not a declared var — declare it`,
		`error [C301]: contract "c": output "url": from: names node "build", which the program does not declare`,
		`error [C300]: contract "c": version 0 — a public contract version starts at 1`,
		`error [C301]: contract "c": output "url" names no producer`,
		`error [C302]: contract "c": criterion "k": parameter "min" must be an integer`,
	}
	got := fragmentShapeErrors(errs)
	want := errs[2:]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}
