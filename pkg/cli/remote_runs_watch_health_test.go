package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The human line names the lease holder the server reports, and says what the
// store answered when it names none — an operator reads who holds the sweep's
// lease without hitting the route until the holder answers (#2680).
func TestPrintRemoteWatchHealth_NamesTheLeaseHolder(t *testing.T) {
	until := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		c    remoteWatchHealthCoordinator
		want string
	}{
		{"held", remoteWatchHealthCoordinator{Present: true, Holder: "iterion-abc-1", HolderExpiresAt: &until, HolderState: "held"}, "holder=iterion-abc-1 "},
		{"expired", remoteWatchHealthCoordinator{Present: true, Holder: "iterion-abc-1", HolderExpiresAt: &until, HolderState: "expired", Stale: true}, "holder=iterion-abc-1 (expired) "},
		{"free", remoteWatchHealthCoordinator{Present: true, HolderState: "free"}, "holder=none "},
		{"unreadable", remoteWatchHealthCoordinator{Present: true, HolderState: "unreadable"}, "holder=unreadable "},
		{"older server", remoteWatchHealthCoordinator{Present: true}, "holder=unknown "},
		{"newer server, a state this CLI predates", remoteWatchHealthCoordinator{Present: true, Holder: "iterion-abc-1", HolderExpiresAt: &until, HolderState: "contested"}, "holder=iterion-abc-1 (contested) "},
		{"newer server, a nameless state", remoteWatchHealthCoordinator{Present: true, HolderState: "contested"}, "holder=none (contested) "},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		p := &Printer{W: &buf, Format: OutputHuman}
		printRemoteWatchHealth(p, remoteWatchHealth{RunID: "run-1", Coordinator: tc.c}, false)
		got := buf.String()
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%s: human output lacks %q:\n%s", tc.name, tc.want, got)
		}
		if hasUntil := strings.Contains(got, "Lease until"); hasUntil != (tc.c.HolderExpiresAt != nil) {
			t.Fatalf("%s: Lease until printed=%t, expiry sent=%t:\n%s", tc.name, hasUntil, tc.c.HolderExpiresAt != nil, got)
		}
	}

	var decoded remoteWatchHealth
	if err := json.Unmarshal([]byte(`{"coordinator":{"present":true,"elected":false,"holder":"iterion-abc-1","holder_expires_at":"2026-10-22T09:00:00Z","holder_state":"held","stale":false}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coordinator.Holder != "iterion-abc-1" || decoded.Coordinator.HolderState != "held" || decoded.Coordinator.HolderExpiresAt == nil || !decoded.Coordinator.HolderExpiresAt.Equal(until) {
		t.Fatalf("the server's holder fields did not decode: %+v", decoded.Coordinator)
	}
}

// --follow prints on change: a renewal moves the lease expiry every third
// of the TTL and is not a change; a new holder is.
func TestWatchHealthChangeKey_IgnoresTheLeaseRenewal(t *testing.T) {
	t1 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(20 * time.Second)
	a := remoteWatchHealth{RunID: "run-1", Coordinator: remoteWatchHealthCoordinator{Present: true, Holder: "pod-1", HolderState: "held", HolderExpiresAt: &t1}}
	b := a
	b.Coordinator.HolderExpiresAt = &t2
	if watchHealthChangeKey(a) != watchHealthChangeKey(b) {
		t.Fatal("a renewed expiry reads as a change")
	}
	c := a
	c.Coordinator.Holder = "pod-2"
	if watchHealthChangeKey(a) == watchHealthChangeKey(c) {
		t.Fatal("a new holder does not read as a change")
	}
	if a.Coordinator.HolderExpiresAt == nil {
		t.Fatal("the key computation modified its argument")
	}
}

// --follow prints a changed answer, a heartbeat after a minute, and a line
// whose printed lease expiry has passed — never a bare renewal.
func TestWatchHealthReprint_RefreshesAPassedExpiryAndNothingElse(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	ahead, passed := now.Add(40*time.Second), now.Add(-time.Second)
	cases := []struct {
		name                       string
		prev, key                  string
		printed                    *time.Time
		since                      time.Duration
		wantReprint, wantHeartbeat bool
	}{
		{"a changed answer", "a", "b", &ahead, 5 * time.Second, true, false},
		{"unchanged, a minute later", "a", "a", &ahead, time.Minute, true, true},
		{"unchanged, the printed expiry passed", "a", "a", &passed, 10 * time.Second, true, false},
		{"unchanged, the printed expiry ahead", "a", "a", &ahead, 10 * time.Second, false, false},
		{"unchanged, no expiry printed", "a", "a", nil, 10 * time.Second, false, false},
	}
	for _, tc := range cases {
		reprint, heartbeat := watchHealthReprint(tc.prev, tc.key, tc.printed, tc.since, now)
		if reprint != tc.wantReprint || heartbeat != tc.wantHeartbeat {
			t.Errorf("%s: reprint=%t heartbeat=%t, want %t %t", tc.name, reprint, heartbeat, tc.wantReprint, tc.wantHeartbeat)
		}
	}
}

// The handover line is the sick-but-alive leader made readable (#2685): what
// the mechanism is set to, where this replica stands in it, and the last pass
// it classified. A server older than the field sends none, and the line is
// omitted rather than printed as a healthy zero.
func TestPrintRemoteWatchHealth_ReportsTheHandover(t *testing.T) {
	at := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		h    *remoteWatchHandover
		want []string
	}{
		{
			name: "a healthy armed replica",
			h:    &remoteWatchHandover{Armed: true, Passes: 3, LastPassVerdict: "idle", LastPassAt: &at},
			want: []string{"failing passes=0/3", "handovers=0", "last pass=idle"},
		},
		{
			name: "a replica mid-run of failures",
			h:    &remoteWatchHandover{Armed: true, Passes: 3, FailingPasses: 2, Handovers: 1, SickTerms: 1, LastPassVerdict: "unreachable", LastPassAt: &at},
			want: []string{"failing passes=2/3", "handovers=1", "sick terms=1", "last pass=unreachable"},
		},
		{
			name: "a replica that stopped handing over",
			h:    &remoteWatchHandover{Armed: true, Passes: 3, FailingPasses: 3, Handovers: 3, SickTerms: 3, CircuitOpen: true, LastPassVerdict: "unreachable"},
			want: []string{"HANDING OVER STOPPED", "every peer gave the lease back just as sick"},
		},
		{
			name: "the operator disarmed it",
			h:    &remoteWatchHandover{Armed: false},
			want: []string{"disarmed by the operator"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := &Printer{W: &buf, Format: OutputHuman}
			printRemoteWatchHealth(p, remoteWatchHealth{RunID: "run-1", Coordinator: remoteWatchHealthCoordinator{Present: true, Elected: true, HolderState: "held", Handover: tc.h}}, false)
			got := buf.String()
			if !strings.Contains(got, "Handover") {
				t.Fatalf("no handover line:\n%s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("handover line lacks %q:\n%s", want, got)
				}
			}
		})
	}

	// A server that predates the field: no line at all, rather than a zero
	// state an operator would read as "armed and healthy".
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}
	printRemoteWatchHealth(p, remoteWatchHealth{RunID: "run-1", Coordinator: remoteWatchHealthCoordinator{Present: true, HolderState: "held"}}, false)
	if got := buf.String(); strings.Contains(got, "Handover") {
		t.Fatalf("an older server's answer printed a handover line:\n%s", got)
	}

	// And it decodes from what the server actually sends.
	var decoded remoteWatchHealth
	if err := json.Unmarshal([]byte(`{"coordinator":{"present":true,"elected":true,"holder_state":"held","stale":false,`+
		`"handover":{"armed":true,"passes":3,"failing_passes":2,"handovers":1,"sick_terms":1,"circuit_open":true,`+
		`"last_pass_at":"2026-10-29T09:00:00Z","last_pass_verdict":"unreachable"}}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	h := decoded.Coordinator.Handover
	if h == nil || !h.Armed || h.Passes != 3 || h.FailingPasses != 2 || h.Handovers != 1 || h.SickTerms != 1 || !h.CircuitOpen ||
		h.LastPassVerdict != "unreachable" || h.LastPassAt == nil || !h.LastPassAt.Equal(at) {
		t.Fatalf("the server's handover fields did not decode: %+v", h)
	}
}
