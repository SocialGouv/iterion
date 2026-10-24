// Package floortest derives, for a syntax-floor test, the release below a
// floor from the floor itself.
//
// A syntax floor (bundle.SyntaxFloors — parser.ProfileSince, ImportSince,
// ContractSince, VarMatchingSince, bundle.ToolAliasesSince,
// DeclaredEmptyToolsSince) is rewritten to the version being released when
// the lot that ships its syntax is cut (internal/floorsalign), so two floors
// cut in the same release carry the same number. A probe that writes "a
// floor below the one this bundle needs" as a lower pin writes the floor
// itself that day, and asserts nothing: Below reads one floor and nothing
// else, and EachPinOrder runs a probe against the collapsed order too.
package floortest

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Below is a release strictly under floor, derived from floor alone: its
// last non-zero component decremented, whatever every other pin names. A
// floor that is not a dotted numeric version, that has no component to
// decrement, or whose decrement does not read back as a release under it, is
// refused rather than turned into a version whose order is a guess.
// For "next", every numeric release is below it, so the probe is 0.0.0.
func Below(t *testing.T, floor string) string {
	t.Helper()
	low, err := below(floor)
	if err != nil {
		t.Fatalf("floortest.Below: %v", err)
	}
	return low
}

// EachPinOrder runs fn under each pin order that differs: once with the pins
// as this tree numbers them, once with the newest profile's pin naming
// release — the state a release that cuts two syntax floors at once leaves
// behind. An expectation that rests on WHICH pin carries a release, rather
// than on what the sources need, holds in the first and fails in the second.
//
// A tree whose newest profile's pin already names release carries that state
// live: there is one order, fn runs under it, and the reason is logged. That
// is the day the aligner cuts a profile lot and the lot that ships release
// together — the state this helper exists to reproduce, so it is not a
// reason to refuse to run.
func EachPinOrder(t *testing.T, release string, fn func(t *testing.T)) {
	t.Helper()
	type order struct {
		name     string
		collapse bool
	}
	orders := []order{{"as pinned", false}, {"two floors cut in one release", true}}
	if parser.ProfileSince[parser.MaxProfile] == release {
		t.Logf("parser.ProfileSince[%d] already names %s: the live pins are the collapsed order, and there is no second order to run", parser.MaxProfile, release)
		orders = []order{{"two floors cut in one release", false}}
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			if order.collapse && !CollapseProfilePin(t, release) {
				t.Fatalf("floortest.EachPinOrder: collapsing onto %s moved no pin: this order would repeat the one it is paired with", release)
			}
			fn(t)
		})
	}
}

// CollapseProfilePin makes the pin the floor table reads for the newest
// profile — parser.ProfileSince[parser.MaxProfile] — name release for the
// rest of the test, and restores it on return: the state a release that cuts
// two syntax floors at once leaves behind, where the aligner writes the
// version being released into every pin it realigns. ProfileSince is a map,
// so it is the floor a test can repoint; its siblings are constants.
//
// It reports whether the pin moved: a pin that already names release is left
// alone, since the live pins then carry the state the collapse simulates. A
// caller that needs two distinct orders reads the result and says so
// (EachPinOrder does).
//
// The floor predicate reads the pin live (bundle.RequiredRelease), so the
// collapse reaches what a bundle is asked for; bundle.SyntaxFloors projects a
// table built at package init and keeps the number the pin had then.
func CollapseProfilePin(t *testing.T, release string) bool {
	t.Helper()
	if _, err := components(release); err != nil && release != "next" {
		t.Fatalf("floortest.CollapseProfilePin: %v", err)
	}
	profile := parser.MaxProfile
	prev, had := parser.ProfileSince[profile]
	if had && prev == release {
		return false
	}
	t.Cleanup(func() {
		if had {
			parser.ProfileSince[profile] = prev
			return
		}
		delete(parser.ProfileSince, profile)
	})
	parser.ProfileSince[profile] = release
	return true
}

// below decrements a version's last non-zero component, keeping the shape it
// was given: 3.150.0 → 3.149.0, 3.150.4 → 3.150.3, 4.0.0 → 3.0.0. What the
// decrement produced is read back and ordered against the floor it came
// from, so what a call site splices into a manifest is a release under that
// floor or an error — never a string the floor predicate reads as nothing.
func below(version string) (string, error) {
	if version == "next" {
		return "0.0.0", nil // Every numeric release is strictly below an uncut pin.
	}
	parts, err := components(version)
	if err != nil {
		return "", err
	}
	floor := append([]int(nil), parts...)
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == 0 {
			continue
		}
		parts[i]--
		out := make([]string, len(parts))
		for j, p := range parts {
			out[j] = strconv.Itoa(p)
		}
		low := strings.Join(out, ".")
		read, rerr := components(low)
		if rerr != nil {
			return "", fmt.Errorf("the release under %q reads back as %q: %w", version, low, rerr)
		}
		if !earlier(read, floor) {
			return "", fmt.Errorf("the release derived from %q is %q, which does not sit below it", version, low)
		}
		return low, nil
	}
	return "", fmt.Errorf("version %q has no non-zero component: no release sits below it", version)
}

// earlier orders two component lists; a result of another length than the
// floor it came from is not ordered against it at all.
func earlier(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// components splits a version into its dotted numeric components, the shape
// bundle.CompareVersions orders. Below takes a pin as the pins are spelled —
// bare dotted numbers: a `v` prefix, a `+build` tail or a sign, each of
// which the engine-constraint grammar tolerates around a version
// (bundle.ParseEngineConstraint), is refused rather than carried into the
// result. The prefix a call site needs stays at the call site.
func components(version string) ([]int, error) {
	fields := strings.Split(strings.TrimSpace(version), ".")
	parts := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if !digits(f) || err != nil {
			return nil, fmt.Errorf("version %q is not a dotted numeric version (3.150.0)", version)
		}
		parts = append(parts, n)
	}
	return parts, nil
}

// digits reports whether s is a non-empty run of ASCII digits: what
// strconv.Atoi accepts, minus the sign it also takes.
func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
