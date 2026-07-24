package tracker

import (
	"reflect"
	"testing"
)

func TestParseBlockerRefs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []int
	}{
		{"none", "Just a normal issue body with no deps.", nil},
		{"blocked by single", "Blocked by #41\n", []int{41}},
		{"depends on single", "Depends on #7", []int{7}},
		{"case-insensitive", "BLOCKED BY #9", []int{9}},
		{"multiple refs one line", "Blocked by #1, #2 and #3", []int{1, 2, 3}},
		{"multiple lines dedup", "Blocked by #5\nDepends on #5\nDepends on #6", []int{5, 6}},
		{"markdown markers", "> Blocked by #12\n- Depends on #13", []int{12, 13}},
		{"mid-sentence is ignored", "This is not blocked by anything really", nil},
		{"keyword without ref", "Blocked by a design decision", nil},
		{"ref elsewhere ignored", "See #99 for context.\nCloses #100", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseBlockerRefs(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseBlockerRefs(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestHeldByOpenBlockers(t *testing.T) {
	open := map[int]bool{41: true, 42: true} // #41,#42 still open; #40 closed/absent

	cases := []struct {
		name string
		body string
		want []int
	}{
		{"no blockers", "nothing here", nil},
		{"open blocker holds", "Blocked by #41", []int{41}},
		{"closed blocker frees (fail-open)", "Blocked by #40", nil},
		{"unresolvable ref frees (fail-open)", "Blocked by #9999", nil},
		{"mix — only open ones held, sorted", "Blocked by #42\nDepends on #40, #41", []int{41, 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HeldByOpenBlockers(tc.body, open); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("HeldByOpenBlockers(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
