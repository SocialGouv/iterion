package server

import "time"

// optRFC3339 returns the RFC3339 form of t as a *string, or nil when
// t is nil. Used by the auth/byok/oauth view marshallers to expose
// optional timestamps (last-used, last-refreshed, expiry) without
// the inline `if t != nil { s := t.Format(...); return &s }` pattern.
func optRFC3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
