package server

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/audit"
)

// defaultAuditInsertTimeout bounds one background audit insert from the
// moment auditInsert queues it: the line reaches the store within it, or
// never.
const defaultAuditInsertTimeout = 5 * time.Second

// auditInsertSettleDivisor sets the margin past the insert bound that an
// erasure's second scrub waits — a fifth of the bound, 1 s by default — for a
// write the store applies just after its client gave up.
const auditInsertSettleDivisor = 5

// auditInsertBound is the bound every background audit insert of this
// process is held to (s.auditInsertTimeout, the default when unset). Every
// replica runs the same code, so the bound holds for the lines queued on
// any of them.
func (s *Server) auditInsertBound() time.Duration {
	if s.auditInsertTimeout > 0 {
		return s.auditInsertTimeout
	}
	return defaultAuditInsertTimeout
}

// auditInsertNow is auditInsert written synchronously, for the one line whose
// loss the caller must report: it stamps the id and the time and answers the
// store's error.
func (s *Server) auditInsertNow(ctx context.Context, e audit.Event) error {
	if s.auditStore == nil {
		return nil
	}
	e.ID = uuid.NewString()
	e.CreatedAt = time.Now().UTC()
	if err := s.auditStore.Insert(ctx, e); err != nil {
		return fmt.Errorf("audit: insert %s: %w", e.Action, err)
	}
	return nil
}

// waitOutAuditInserts returns once every audit line queued — on any replica
// — before since has reached the store or given up: since plus the insert
// bound and its settle margin. It answers ctx's error when ctx ends first.
func (s *Server) waitOutAuditInserts(ctx context.Context, since time.Time) error {
	bound := s.auditInsertBound()
	wait := time.Until(since.Add(bound + bound/auditInsertSettleDivisor))
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("audit: wait out the lines still in flight: %w", ctx.Err())
	}
}
