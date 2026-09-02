package runner

import "testing"

func TestRunnerEpochAdmissionRelation(t *testing.T) {
	for _, tc := range []struct {
		self    uint64
		message uint64
		want    bool
	}{
		{self: 2, message: 0, want: true},
		{self: 2, message: 1, want: true},
		{self: 2, message: 2, want: true},
		{self: 2, message: 3, want: false},
	} {
		if got := runnerEpochAccepted(tc.self, tc.message); got != tc.want {
			t.Errorf("self=%d message=%d accepted=%t, want %t", tc.self, tc.message, got, tc.want)
		}
	}
}
