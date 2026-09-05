package main

import "testing"

// The kill switch must read every falsy spelling the repo's other switches
// take — a value that "looks set" but keeps rebranding accounts is worse
// than none.
func TestForgeBrandAvatarDisabled(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{{"", false}, {"on", false}, {"1", false}, {"off", true}, {" OFF ", true}, {"0", true}, {"false", true}, {"no", true}, {"NO", true}} {
		t.Setenv("ITERION_FORGE_BRAND_AVATAR", tc.v)
		if got := forgeBrandAvatarDisabled(); got != tc.want {
			t.Errorf("ITERION_FORGE_BRAND_AVATAR=%q → disabled=%v, want %v", tc.v, got, tc.want)
		}
	}
}
