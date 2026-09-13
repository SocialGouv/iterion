package store

import "context"

// VeilleChannel names WHICH standby an assistant-veille event describes. The
// dock needs it because the two are stopped through different calls and a
// button that cuts only one leaves the other armed: a card veille re-arms a
// run watch at the card's next dispatch, so "stop watching" would not stick.
type VeilleChannel string

const (
	// VeilleChannelIssue is the standing subscription to a board card's
	// transitions (Run.WatchedIssueIDs).
	VeilleChannelIssue VeilleChannel = "issue"
	// VeilleChannelRun is one link to a single target run's outcome
	// (pkg/runwatch).
	VeilleChannelRun VeilleChannel = "run"
)

// VeilleEvent builds the observational record of an assistant starting or
// stopping a standby. runID is the ASSISTANT's run — the one whose transcript
// must explain why it is quiet now and why it will speak later.
func VeilleEvent(typ EventType, channel VeilleChannel, subject, reason string) Event {
	data := map[string]any{"channel": string(channel), "subject": subject}
	if reason != "" {
		data["reason"] = reason
	}
	return Event{Type: typ, Data: data}
}

// PublishVeilleArmed / PublishVeilleStopped are the two calls every arming and
// stopping site goes through. They are deliberately the ONLY producers of
// these events: pkg/server alone may stop a watch when its target disappears,
// its assistant ends, the target reaches Done, or the operator presses Stop;
// hand-written emissions at every site would drift within a release.
func PublishVeilleArmed(ctx context.Context, s RunStore, publish func(Event), assistantRunID string, channel VeilleChannel, subject string) {
	if s == nil || assistantRunID == "" {
		return
	}
	AppendAndPublish(ctx, s, publish, assistantRunID, VeilleEvent(EventAssistantVeilleArmed, channel, subject, ""))
}

func PublishVeilleStopped(ctx context.Context, s RunStore, publish func(Event), assistantRunID string, channel VeilleChannel, subject, reason string) {
	if s == nil || assistantRunID == "" {
		return
	}
	AppendAndPublish(ctx, s, publish, assistantRunID, VeilleEvent(EventAssistantVeilleStopped, channel, subject, reason))
}
