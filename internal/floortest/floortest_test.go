package floortest

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Below sits strictly under every floor the registry names — the real pins,
// at the numbers this tree carries — so a probe that asks for a floor below
// the one it needs gets one whatever the release cut has written into the
// others.
func TestBelowSitsStrictlyUnderEveryPinOfTheRegistry(t *testing.T) {
	floors := bundle.SyntaxFloors()
	if len(floors) == 0 {
		t.Fatal("the floor registry names no pin")
	}
	for name, release := range floors {
		low := Below(t, release)
		cmp, ok := bundle.CompareVersions(low, release)
		if !ok {
			t.Errorf("%s: Below(%q) = %q, which cannot be ordered against the floor", name, release, low)
			continue
		}
		if cmp >= 0 {
			t.Errorf("%s: Below(%q) = %q, not below the floor", name, release, low)
		}
	}
}

// Two pins naming one release — two floors cut in the same release — leave
// Below where it was: it reads the floor given and no other pin. The pin the
// collapse repoints is the one the floor predicate reads for the newest
// profile; the registry, a projection of a table built at init, carries the
// number it started with.
func TestBelowIgnoresWhatTheOtherPinsName(t *testing.T) {
	want := Below(t, parser.ImportSince)
	pin := fmt.Sprintf("parser.ProfileSince[%d]", parser.MaxProfile)
	registered := bundle.SyntaxFloors()[pin]
	if !CollapseProfilePin(t, parser.ImportSince) {
		t.Logf("%s already names the import release: the two pins this case needs on one release are live", pin)
	}
	if got := Below(t, parser.ImportSince); got != want {
		t.Errorf("Below(ImportSince) = %q with %s on the same release, %q before", got, pin, want)
	}
	if got := parser.ProfileSince[parser.MaxProfile]; got != parser.ImportSince {
		t.Errorf("the collapse did not reach the pin the floor table reads: %s = %q", pin, got)
	}
	if got := bundle.SyntaxFloors()[pin]; got != registered {
		t.Errorf("the registry now follows the pin (%q, %q before): CollapseProfilePin's godoc says it does not — update it", got, registered)
	}
}

// The collapse lasts as long as the test that asked for it: a pin left
// repointed would decide, in silence, what a later test is asked for. The
// release collapsed onto is derived from the pin itself, so the pin cannot
// already name it and the move is certain.
func TestCollapseProfilePinRestoresThePin(t *testing.T) {
	want := parser.ProfileSince[parser.MaxProfile]
	target := Below(t, want)
	t.Run("collapsed", func(t *testing.T) {
		if !CollapseProfilePin(t, target) {
			t.Fatalf("the pin %q was left alone by a collapse onto %q, a release below it", want, target)
		}
		if parser.ProfileSince[parser.MaxProfile] != target {
			t.Fatalf("the pin = %q, want the collapsed %q", parser.ProfileSince[parser.MaxProfile], target)
		}
	})
	if got := parser.ProfileSince[parser.MaxProfile]; got != want {
		t.Errorf("the pin = %q after the test that collapsed it, want %q", got, want)
	}
}

// A release the pin already names is left alone and said so: nothing is
// collapsed because the live pins already are the collapsed order, which is
// the state a release cutting two syntax floors at once leaves.
func TestCollapseProfilePinLeavesAPinThatAlreadyNamesTheRelease(t *testing.T) {
	live := parser.ProfileSince[parser.MaxProfile]
	if CollapseProfilePin(t, live) {
		t.Fatalf("a collapse onto %q, the release the pin already names, reports a move", live)
	}
	if got := parser.ProfileSince[parser.MaxProfile]; got != live {
		t.Errorf("the pin = %q after a collapse that moved nothing, want %q", got, live)
	}
}

// EachPinOrder runs its probe twice, and the second run is a real collapse:
// the pin the floor table reads for the newest profile names the release
// given. A probe held only against the order this tree carries would pass
// over the very day the cut puts two floors on one release. The release is
// derived from the live pin, so the two orders are certain to differ.
func TestEachPinOrderRunsTheProbeInBothOrders(t *testing.T) {
	target := Below(t, parser.ProfileSince[parser.MaxProfile])
	seen := map[string]string{}
	EachPinOrder(t, target, func(t *testing.T) {
		seen[t.Name()] = parser.ProfileSince[parser.MaxProfile]
	})
	if len(seen) != 2 {
		t.Fatalf("EachPinOrder ran %d orders: %v", len(seen), seen)
	}
	var collapsed int
	for _, pin := range seen {
		if pin == target {
			collapsed++
		}
	}
	if collapsed != 1 {
		t.Fatalf("%d of the orders carried the collapsed pin, want exactly one: %v", collapsed, seen)
	}
}

// A release the newest profile's pin already names leaves one order to run,
// and EachPinOrder runs the probe under it — the live pins ARE the collapsed
// order. That is the state the aligner leaves when a profile lot and the lot
// that ships another floor are cut in one release, and the state a probe
// written for a PENDING pin passes here: refusing it would make the helper
// die on the only day it is about.
func TestEachPinOrderRunsOnceWhenThePinsAlreadyCarryTheCollapse(t *testing.T) {
	live := parser.ProfileSince[parser.MaxProfile]
	seen := map[string]string{}
	EachPinOrder(t, live, func(t *testing.T) {
		seen[t.Name()] = parser.ProfileSince[parser.MaxProfile]
	})
	if len(seen) != 1 {
		t.Fatalf("EachPinOrder ran %d orders for %q, the release the pin already names: %v", len(seen), live, seen)
	}
	for name, pin := range seen {
		if pin != live {
			t.Errorf("%s ran with the pin at %q, want the live %q", name, pin, live)
		}
	}
}

// The arithmetic decrements the last non-zero component, so the result keeps
// the shape of the floor it came from and crosses a zero component rather
// than naming a version that does not order.
func TestBelowDecrementsTheLastNonZeroComponent(t *testing.T) {
	for _, tc := range []struct{ floor, want string }{
		{"3.150.0", "3.149.0"},
		{"3.150.4", "3.150.3"},
		{"3.145", "3.144"},
		{"4.0.0", "3.0.0"},
		{"0.0.1", "0.0.0"},
	} {
		got, err := below(tc.floor)
		if err != nil || got != tc.want {
			t.Errorf("below(%q) = %q, %v; want %q", tc.floor, got, err, tc.want)
		}
	}
}

// A floor Below cannot order, or that has nothing under it, is refused
// instead of yielding a version whose order is a guess. A signed component
// is one of those: strconv.Atoi takes a sign, the engine's own comparator
// does not, so `+3.150.0` would get an answer no floor predicate can read.
func TestBelowRefusesAFloorItCannotOrder(t *testing.T) {
	for _, floor := range []string{
		"", "0.0.0", "latest", "3.2.0-rc1", "v3.150.0", "3.150.0+abc123",
		"+3.150.0", "3.+1.0", "-3.150.0", "3.-1.0", "99999999999999999999.0.0",
	} {
		if got, err := below(floor); err == nil {
			t.Errorf("below(%q) = %q, want a refusal", floor, got)
		}
	}
}

// What the pins actually are: bare dotted numbers, the shape Below takes. A
// pin spelled any other way would make every floor case read a release the
// arithmetic refuses, and say so at the first probe.
func TestEveryPinOfTheRegistryIsAShapeBelowTakes(t *testing.T) {
	for name, release := range bundle.SyntaxFloors() {
		if _, err := below(release); err != nil {
			t.Errorf("%s = %q: %v", name, release, err)
		}
	}
}
