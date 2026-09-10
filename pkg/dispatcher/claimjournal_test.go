package dispatcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func quietLogger() *iterlog.Logger { return iterlog.New(iterlog.LevelError, &bytes.Buffer{}) }

func TestClaimJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j := newClaimJournal(dir, quietLogger())
	j.Record(claimEntry{IssueID: "gh:1", Identifier: "gh#1", Marker: "rog-42", ClaimedAt: time.Now().UTC()})
	j.Record(claimEntry{IssueID: "gh:2", Identifier: "gh#2", Marker: "rog-42", ClaimedAt: time.Now().UTC()})
	if !j.Contains("gh:1") || !j.Contains("gh:2") || j.Contains("gh:missing") {
		t.Fatal("Contains did not reflect journalled claims")
	}
	j.Remove("gh:1")
	if j.Contains("gh:1") {
		t.Fatal("Contains retained a removed claim")
	}

	// A fresh journal (successor daemon) sees exactly the un-released claim.
	j2 := newClaimJournal(dir, quietLogger())
	got := j2.Load()
	if len(got) != 1 || got[0].IssueID != "gh:2" || got[0].Marker != "rog-42" {
		t.Fatalf("reloaded journal = %+v, want the one un-released claim gh:2", got)
	}
}

func TestClaimJournalNilAndCorrupt(t *testing.T) {
	// Store-less dispatcher: nil journal, all methods no-op.
	var j *claimJournal
	j.Record(claimEntry{IssueID: "x"})
	j.Remove("x")
	if j.Contains("x") {
		t.Fatal("nil journal Contains must be false")
	}
	if got := j.Load(); got != nil {
		t.Fatalf("nil journal Load = %v, want nil", got)
	}
	// Corrupt file must not wedge startup.
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatcher", "claims.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	j2 := newClaimJournal(dir, quietLogger())
	if got := j2.Load(); len(got) != 0 {
		t.Fatalf("corrupt journal Load = %v, want empty", got)
	}
}

// TestSweepJournalledClaims is the crash-recovery regression for
// external trackers: a journal entry left by a dead local PID gets its
// tracker claim released at the next boot; entries from live PIDs (a
// peer daemon on the same store dir) stay claimed.
func TestSweepJournalledClaims(t *testing.T) {
	host, _ := osHostname()
	if host == "" {
		host = "dispatcher"
	}
	deadMarker := host + "-999999"
	liveMarker := host + "-" + strconv.Itoa(os.Getpid())

	dir := t.TempDir()
	seed := newClaimJournal(dir, quietLogger())
	seed.Record(claimEntry{IssueID: "gh:dead", Identifier: "gh#dead", Marker: deadMarker, ClaimedAt: time.Now().UTC()})
	seed.Record(claimEntry{IssueID: "gh:live", Identifier: "gh#live", Marker: liveMarker, ClaimedAt: time.Now().UTC()})

	// The fake tracker does NOT implement SweepStaleClaims — exactly the
	// external-adapter shape the journal sweep exists for.
	ft := newFakeTracker()
	ft.mu.Lock()
	ft.claims["gh:dead"] = deadMarker
	ft.claims["gh:live"] = liveMarker
	ft.mu.Unlock()

	c := newTestDispatcher(t, &StubRunner{Handler: func(context.Context, DispatchSpec) error { return nil }}, ft, time.Hour)
	c.claims = newClaimJournal(dir, quietLogger())

	c.sweepStaleLocalClaims()

	ft.mu.Lock()
	_, deadStillClaimed := ft.claims["gh:dead"]
	_, liveStillClaimed := ft.claims["gh:live"]
	ft.mu.Unlock()
	if deadStillClaimed {
		t.Fatal("dead-PID claim was not released by the journal sweep")
	}
	if !liveStillClaimed {
		t.Fatal("live-PID claim was released — a peer daemon's claim must stay")
	}
	// Journal reflects the sweep: the dead entry is gone, the live stays.
	left := c.claims.Load()
	if len(left) != 1 || left[0].IssueID != "gh:live" {
		t.Fatalf("journal after sweep = %+v, want only gh:live", left)
	}
}

// TestReleaseClaimDropsJournalEntry pins the release path: once
// finishRun releases the tracker claim, the journal entry must go too,
// else the next boot would "recover" a claim that no longer exists.
func TestReleaseClaimDropsJournalEntry(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{Handler: func(context.Context, DispatchSpec) error { return nil }}, ft, time.Hour)
	c.claims = newClaimJournal(t.TempDir(), quietLogger())

	c.claims.Record(claimEntry{IssueID: "gh:1", Identifier: "gh#1", Marker: c.hostMarker, ClaimedAt: time.Now().UTC()})
	if err := ft.Claim(context.Background(), "gh:1", c.hostMarker); err != nil {
		t.Fatal(err)
	}
	c.releaseClaim(context.Background(), "gh:1", "gh#1")
	if got := c.claims.Load(); len(got) != 0 {
		t.Fatalf("journal after releaseClaim = %+v, want empty", got)
	}
}

// A journalled marker that can NEVER be proven dead (unparsable shape,
// pid <= 1) is — on an external tracker, where the journal is the ONLY
// recovery path — a permanently stranded claim label: the boot sweep
// must NAME it, once, instead of declining in silence. A live-pid
// decline (another daemon sharing the store) stays silent: warning on
// it is a storm in every legitimate multi-daemon setup.
func TestJournalSweep_PermanentDeclinesAreNamed(t *testing.T) {
	host, _ := os.Hostname()
	if host == "" {
		host = "dispatcher"
	}
	ft := newFakeTracker()
	var buf bytes.Buffer
	c := &Dispatcher{
		tracker:    ft,
		logger:     iterlog.New(iterlog.LevelWarn, &buf),
		hostMarker: host + "-1",
		claims:     newClaimJournal(t.TempDir(), quietLogger()),
	}
	c.claims.Record(claimEntry{IssueID: "gh:pid1", Identifier: "gh#pid1", Marker: host + "-1", ClaimedAt: time.Now().UTC()})
	c.claims.Record(claimEntry{IssueID: "gh:live", Identifier: "gh#live", Marker: host + "-" + strconv.Itoa(os.Getpid()), ClaimedAt: time.Now().UTC()})

	c.sweepJournalledClaims(host)

	out := buf.String()
	if !strings.Contains(out, "NEVER be proven dead") || !strings.Contains(out, "gh#pid1") {
		t.Fatalf("the permanently-undecidable entry was declined in silence: %q", out)
	}
	if strings.Contains(out, "gh#live") {
		t.Fatalf("a live peer daemon's entry was named as stranded — false-positive storm in multi-daemon setups: %q", out)
	}
}
