package claudesdk

import (
	"testing"
)

// The wire shape is the CLI's own schema (claude 2.1.220):
//
//	{type: "rate_limit_event", rate_limit_info: {status, rateLimitType,
//	 utilization, resetsAt}, uuid, session_id}
//
// Utilization is a FRACTION, resetsAt is Unix SECONDS, and every field but
// status is optional — reading any of those three wrong turns a cap into a
// number that means nothing.
func TestUnmarshalRateLimitEvent(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.92,"resetsAt":1787086800},"uuid":"c0ffee","session_id":"sess-1"}`)

	msg, err := unmarshalMessage(line)
	if err != nil {
		t.Fatalf("unmarshalMessage: %v", err)
	}
	ev, ok := msg.(*RateLimitEvent)
	if !ok {
		t.Fatalf("got %T, want *RateLimitEvent", msg)
	}
	if ev.Info.Status != "allowed_warning" || ev.Info.RateLimitType != "seven_day" {
		t.Errorf("info = %+v", ev.Info)
	}
	if ev.Info.Utilization == nil || *ev.Info.Utilization != 0.92 {
		t.Errorf("utilization = %v, want the raw fraction 0.92", ev.Info.Utilization)
	}
	if ev.Info.ResetsAt == nil || *ev.Info.ResetsAt != 1787086800 {
		t.Errorf("resetsAt = %v, want the epoch seconds verbatim", ev.Info.ResetsAt)
	}
	if ev.SessionID != "sess-1" {
		t.Errorf("session_id = %q", ev.SessionID)
	}
}

// A refusal arrives with a status and nothing else. Defaulting the absent
// utilization to 0 would read as "0% consumed" — the opposite of the truth —
// so the pointer must stay nil and let the policy layer decide.
func TestUnmarshalRateLimitEvent_RejectedWithoutNumbers(t *testing.T) {
	msg, err := unmarshalMessage([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"},"uuid":"u","session_id":"s"}`))
	if err != nil {
		t.Fatalf("unmarshalMessage: %v", err)
	}
	ev := msg.(*RateLimitEvent)
	if ev.Info.Status != "rejected" {
		t.Fatalf("status = %q", ev.Info.Status)
	}
	if ev.Info.Utilization != nil || ev.Info.ResetsAt != nil {
		t.Errorf("absent fields must stay absent, got %+v", ev.Info)
	}
}

// The session read loop drops anything isMessage rejects, so a type missing
// from that list never reaches a consumer however well it parses.
func TestRateLimitEventIsRoutedAsAMessage(t *testing.T) {
	if !isMessage(&rawMessage{Type: "rate_limit_event"}) {
		t.Fatal("rate_limit_event must be routed to the message stream")
	}
}
