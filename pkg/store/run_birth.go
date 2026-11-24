package store

import (
	"context"
	"errors"
)

var ErrRunBirthUnavailable = errors.New("store: atomic owned run birth unavailable")
var ErrRunBirthState = errors.New("store: run birth reservation already used or released")
var ErrRunBirthExpired = errors.New("store: run birth reservation lease expired")

// RunBirthReservation owns preparation outside the published run namespace.
// Publish is single-use and exclusive: an incumbent is never adopted. A nonnil
// Run together with an error means this reservation DID publish that birth but
// could not acknowledge durability/lifetime. Keep that receipt for retirement;
// do not run effects or reinterpret the error as an absent run.
// Unlock is idempotent and never removes a published run. A renewable
// reservation also exposes Refresh(context.Context) error and LeaseTTL().
// This is advisory liveness, not execution authority.
type RunBirthReservation interface {
	RunLock
	Publish(context.Context, RunCreateSpec) (*Run, error)
}

type RunBirthStore interface {
	BeginRunBirth(context.Context, string) (RunBirthReservation, error)
}

func AsRunBirthStore(s RunStore) RunBirthStore { return capability[RunBirthStore](s) }
