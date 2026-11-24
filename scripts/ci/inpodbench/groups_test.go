package main

import (
	"strings"
	"testing"
)

func TestTheGroupCheckRefusesOnlyAWideningWhenNothingIsDeclared(t *testing.T) {
	for _, tc := range []struct {
		name, groups string
		ok           bool
	}{
		{"the chart's own group", "10001", true},
		// Absence widens nothing. The docker arm passes `--user uid:gid`, and
		// it must not refuse its own posture on a runtime that leaves the
		// supplementary set empty.
		{"no supplementary group at all", "", true},
		{"root group added", "0 10001", false},
		{"an image group the chart does not name", "27 10001", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGroups(tc.groups, 10001, nil)
			if tc.ok && err != nil {
				t.Fatalf("Groups %q refused: %v", tc.groups, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Groups %q accepted: a widening passed for the chart's posture", tc.groups)
			}
		})
	}
}

func TestADeclaredGroupSetIsMatchedExactlyInBothDirections(t *testing.T) {
	runner := []int{27, 100, 1000}
	if err := checkGroups("27 100 1000", 1000, runner); err != nil {
		t.Fatalf("the production runner image's real set was refused: %v", err)
	}
	if err := checkGroups("27 100 1000 5", 1000, runner); err == nil {
		t.Fatal("an undeclared observed group passed: the declaration widened by omission")
	}
	err := checkGroups("27 100 1000", 1000, []int{5, 27, 100, 1000})
	if err == nil {
		t.Fatal("a declared group absent from the pod passed: the declaration does not describe it")
	}
	if !strings.Contains(err.Error(), "group 5 is ABSENT") {
		t.Errorf("the refusal does not name the absent group: %v", err)
	}
}

// Several declared groups missing at once: the message must name the same one
// every time, not whichever a map iteration happened to reach first.
func TestTheAbsentGroupNamedDoesNotDependOnMapOrder(t *testing.T) {
	first := ""
	for i := 0; i < 50; i++ {
		err := checkGroups("1000", 1000, []int{900, 7, 42, 1000})
		if err == nil {
			t.Fatal("three absent declared groups passed")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("two runs named different absent groups:\n  %s\n  %s", first, err.Error())
		}
	}
	if !strings.Contains(first, "group 7 is ABSENT") {
		t.Errorf("the lowest absent id is not the one named: %s", first)
	}
}
