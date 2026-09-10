package alert

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The in-process Manager must not drop the typed code the run_failed
// event has carried all along — the local studio's alerts would
// otherwise stay unclassified while the cloud path names the class.
func TestManagerPropagatesEventFailureCode(t *testing.T) {
	sink := &captureSink{}
	m := newTestManager(sink)
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunFailed, NodeID: "agent",
		Timestamp: time.Now(), Data: map[string]any{"error": "anthropic 401", "code": "AUTH_FAILED"}})
	waitFor(t, func() bool { return len(sink.snapshot()) >= 1 })
	got := sink.snapshot()[0]
	if got.FailureCode != "AUTH_FAILED" {
		t.Fatalf("in-process alert dropped the typed code: %q", got.FailureCode)
	}
}

// The tracker sink is the machine consumer par excellence — it must
// carry failure_code like its two sibling renders (WebhookText,
// AsEventData) instead of forcing Sentry queries to parse Reason.
func TestTrackerSinkCarriesFailureCode(t *testing.T) {
	tr := trackerTransport
	trackerTransportOn.Do(func() {
		errtrack.Init(errtrack.Config{
			DSN:       "https://publickey@localhost/1",
			Transport: tr,
			Logger:    iterlog.New(iterlog.LevelError, io.Discard),
		})
	})
	if !errtrack.Enabled() {
		t.Skip("errtrack init raced another binary state")
	}
	before := len(tr.all())
	TrackerSink{}.Notify(context.Background(), Alert{
		Kind: KindRunFailed, RunID: "r1", Reason: "boom", FailureCode: "TIMEOUT",
	})
	evs := tr.all()
	if len(evs) != before+1 {
		t.Fatalf("events = %d, want %d", len(evs), before+1)
	}
	if got := evs[len(evs)-1].Contexts["iterion"]["failure_code"]; got != "TIMEOUT" {
		t.Fatalf("tracker event failure_code = %v, want TIMEOUT", got)
	}
}

// The webhook text annotates the Reason line when there is one, and
// gives the code its own line otherwise — never gluing "[CODE]" onto
// the node or the run name.
func TestWebhookTextFailureCodePlacement(t *testing.T) {
	withReason := Alert{Kind: KindRunFailed, RunName: "digest", Reason: "deadline", FailureCode: "TIMEOUT"}
	if txt := withReason.WebhookText(); !strings.Contains(txt, "Reason: deadline [TIMEOUT]") {
		t.Errorf("reason+code render: %q", txt)
	}
	noReason := Alert{Kind: KindRunFailed, RunName: "digest", NodeID: "synthesize", FailureCode: "TIMEOUT"}
	txt := noReason.WebhookText()
	if !strings.Contains(txt, "\nCode: TIMEOUT") {
		t.Errorf("code without reason must get its own line: %q", txt)
	}
	if strings.Contains(txt, "synthesize [TIMEOUT]") {
		t.Errorf("code glued onto the node line: %q", txt)
	}
}
