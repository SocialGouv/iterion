package dispatcher

import (
	"os"
	"strconv"
	"testing"
)

// TestIsStaleLocalMarker covers the marker-parsing rules: only host-
// matching, syntactically-valid "<host>-<pid>" markers whose PID is
// gone get swept. Anything we can't confidently interpret stays.
func TestIsStaleLocalMarker(t *testing.T) {
	livePid := os.Getpid() // guaranteed alive — we're it
	const deadPid = 999999 // overwhelmingly likely to be unused
	cases := []struct {
		name   string
		marker string
		host   string
		want   bool
	}{
		{"live host PID stays", "rog-" + strconv.Itoa(livePid), "rog", false},
		{"dead host PID swept", "rog-" + strconv.Itoa(deadPid), "rog", true},
		{"other-host PID stays", "rog-" + strconv.Itoa(deadPid), "other", false},
		{"missing PID stays", "rog-", "rog", false},
		{"non-numeric PID stays", "rog-abc", "rog", false},
		{"no dash stays", "rog", "rog", false},
		{"hostname with dashes stays", "rog-dev-" + strconv.Itoa(deadPid), "rog-dev", true},
		{"pid=1 init stays", "rog-1", "rog", false},
	}
	for _, tc := range cases {
		got := isStaleLocalMarker(tc.marker, tc.host)
		if got != tc.want {
			t.Errorf("%s: isStaleLocalMarker(%q, %q) = %v, want %v", tc.name, tc.marker, tc.host, got, tc.want)
		}
	}
}
