package credroute

// The keys a usage event carries its call's Spend under (ADR-161 decision 8):
// written by the process that built the call — the delegate events, their
// `folded_spend` slices and `llm_step_finished` — and read by the spend ledger
// (the slot) and the credential plan's fine table (the fingerprint), so no
// reader re-derives what the call spent from its model and provider word.
// The event's `fingerprint` key is another fact: the session LABEL resume and
// NodesServed key on, never a credential identity.
const (
	EventKeyChannel     = "credential_channel"
	EventKeySlot        = "credential_slot"
	EventKeyFingerprint = "credential_fingerprint"
)

// PutEventKeys writes s onto an event payload: the channel, and the slot and
// fingerprint where s names them (an absent key is "none", never a measured
// empty). The zero Spend writes nothing: the event then carries no keys, and
// its readers keep their derivation for it (ADR-161 decision 9).
func PutEventKeys(data map[string]any, s Spend) {
	if s.Channel == "" {
		return
	}
	data[EventKeyChannel] = string(s.Channel)
	if s.Slot != "" {
		data[EventKeySlot] = s.Slot
	}
	if s.Fingerprint != "" {
		data[EventKeyFingerprint] = s.Fingerprint
	}
}

// SpendFromEvent reads the keys PutEventKeys wrote onto a payload — a live
// event's map, or one decoded back from JSON. ok is false where the payload
// carries no channel: an event from a backend whose arm has not landed, or
// from a binary that predates the keys.
func SpendFromEvent(data map[string]any) (s Spend, ok bool) {
	channel, _ := data[EventKeyChannel].(string)
	if channel == "" {
		return Spend{}, false
	}
	s.Channel = Channel(channel)
	s.Slot, _ = data[EventKeySlot].(string)
	s.Fingerprint, _ = data[EventKeyFingerprint].(string)
	return s, true
}

// RunSlot reports whether the spend books on one of the run's own slots — the
// ledger's and the fine table's question. A spend of the pod's environment, a
// login on the host or a backend's own config names no slot of the run: its
// Fingerprint, where set, is the account the usage meter keys, never a row
// the credential plan predicts.
func (s Spend) RunSlot() bool { return s.Slot != "" }
