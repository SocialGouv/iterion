package auth

import (
	"context"
	"fmt"
	"time"
)

// SessionExpiresAt reads the bound of the refresh session a lineage holds:
// the expires_at of its live session row, as the server holds it now —
// never the expiry of the access token presented beside it, which lives an
// access TTL, not the refresh TTL. A client uses it for what it names: a
// pending sign-out's record expires at it, so the record is dropped only
// once the session it would end is gone by itself.
//
// found is false, with a zero time and a nil error, when the lineage holds
// no live session — a revocation landed between the credential's mint and
// this read — and when lineage is empty (a credential that names no
// session). A read that failed is the error it is: a bound nobody could
// read is not a fact about the session.
func (s *Service) SessionExpiresAt(ctx context.Context, userID, lineage string) (time.Time, bool, error) {
	if lineage == "" {
		return time.Time{}, false, nil
	}
	sess, found, err := s.sessions.LiveLineageSession(ctx, userID, lineage)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("auth: read the session bound of lineage %s: %w", lineage, err)
	}
	if !found {
		return time.Time{}, false, nil
	}
	return sess.ExpiresAt, true, nil
}
