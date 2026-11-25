package alert

import (
	"context"
	"io"
	"strings"
	"testing"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A stalled schedule names the schedule in its headline, not a generic
// run alert, and its webhook body carries the reason that says which row
// field shows the stall.
func TestScheduleStalledRendersAsAScheduleAlert(t *testing.T) {
	a := Alert{Kind: KindScheduleStalled, RunName: "feed-watch", Reason: "schedule sb-1 (bot feed-watch, 0 7 * * *) is overdue"}
	if got, want := a.Title(), "Schedule stalled: feed-watch"; got != want {
		t.Fatalf("Title() = %q, want %q", got, want)
	}
	text := a.WebhookText()
	if !strings.HasPrefix(text, "Schedule stalled: feed-watch\nReason: schedule sb-1") {
		t.Fatalf("WebhookText() = %q, want the headline then the reason", text)
	}
	if got := a.AsEventData()["kind"]; got != "schedule_stalled" {
		t.Fatalf("event kind = %v, want schedule_stalled", got)
	}
}

// The tracker receives a stalled schedule as an incident (warning), never
// as a breadcrumb that is only sent along with some later event.
func TestTrackerSinkReportsAStalledScheduleAsAWarning(t *testing.T) {
	tr := trackerTransport
	trackerTransportOn.Do(func() {
		errtrack.Init(errtrack.Config{
			DSN:       "https://publickey@localhost/1",
			Transport: tr,
			Logger:    iterlog.New(iterlog.LevelError, io.Discard),
		})
	})
	if !errtrack.Enabled() {
		t.Fatal("errtrack.Init returned false with a valid DSN")
	}
	before := len(tr.all())
	TrackerSink{}.Notify(context.Background(), Alert{Kind: KindScheduleStalled, RunName: "feed-watch", Reason: "overdue"})
	errtrack.Flush()
	events := tr.all()[before:]
	if len(events) != 1 {
		t.Fatalf("tracker events = %d, want 1 (a stalled schedule is an incident, not a breadcrumb)", len(events))
	}
	if events[0].Level != sentry.LevelWarning || events[0].Message != "run alert: schedule_stalled" {
		t.Fatalf("tracker event = (%q, %q), want (warning, run alert: schedule_stalled)", events[0].Level, events[0].Message)
	}
}
