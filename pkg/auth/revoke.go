package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrPasswordChangedSignIn answers a credential change whose new password was
// committed but whose new session could not be opened: the change is done,
// every session it ended stays ended, and the caller signs in with the new
// password. Its message is the one the caller is shown; the cause is wrapped
// beside it for the log (passwordChangedSignIn).
var ErrPasswordChangedSignIn = errors.New("password changed, but no session could be opened: sign in with the new password")

// ErrCredentialChangeBusy answers a credential change of an account another
// change of the same account still holds (ClaimCredentialChange) past the
// wait: the caller presents the request again.
var ErrCredentialChangeBusy = errors.New("auth: another password change of this account is in progress: try again in a moment")

// passwordChangedSignIn wraps cause as ErrPasswordChangedSignIn.
func passwordChangedSignIn(cause error) error {
	return fmt.Errorf("%w: %w", ErrPasswordChangedSignIn, cause)
}

// revocationRecordTimeout bounds the record of a revocation — the one write a
// revocation's caller waits for and answers from.
const revocationRecordTimeout = sessionOpTimeout

// revocationPassAttempts and revocationPassBackoff pace the detached pass of
// a revocation: each attempt is bounded by revocationTimeout, and a pass that
// fails every attempt is logged — its record already refuses what it did not
// reach, and the next revocation of the same lineage or account runs it again.
const revocationPassAttempts = 3

var revocationPassBackoff = []time.Duration{time.Second, 4 * time.Second}

// revokeAccount records an account revocation — the generation raise, which
// is the revocation's commit point — and returns the generation it raised the
// account to; the pass over the account's live sessions runs detached
// (runPass). A record that fails is returned: nothing was revoked.
func (s *Service) revokeAccount(ctx context.Context, userID string) (int64, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revocationRecordTimeout)
	defer cancel()
	now := s.now().UTC()
	generation, err := s.sessions.RecordAccountRevocation(rctx, userID, now)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke every session of user %s: %w", userID, err)
	}
	s.runPass(ctx, fmt.Sprintf("the sessions of user %s below generation %d", userID, generation), func(pctx context.Context) (int64, error) {
		return s.sessions.RevokeAccountSessions(pctx, userID, generation, now)
	})
	return generation, nil
}

// revokeLineage records a lineage revocation — its commit point — and runs
// the pass over the lineage's live sessions detached (runPass). A record that
// fails is returned: nothing was revoked.
func (s *Service) revokeLineage(ctx context.Context, userID, lineageID string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revocationRecordTimeout)
	defer cancel()
	now := s.now().UTC()
	if err := s.sessions.RecordLineageRevocation(rctx, userID, lineageID, now); err != nil {
		return err
	}
	s.runPass(ctx, fmt.Sprintf("the sessions of lineage %s", lineageID), func(pctx context.Context) (int64, error) {
		return s.sessions.RevokeLineageSessions(pctx, userID, lineageID, now)
	})
	return nil
}

// runPass runs the pass of a recorded revocation off the caller's path, under
// the caller's context values but not its cancellation: up to
// revocationPassAttempts attempts, each bounded by revocationTimeout, paced by
// revocationPassBackoff. The pass is idempotent and no answer depends on it —
// the record refuses every session it covers — so a pass that fails every
// attempt is logged and left to the next revocation of the same scope.
func (s *Service) runPass(ctx context.Context, what string, pass func(context.Context) (int64, error)) {
	parent := context.WithoutCancel(ctx)
	s.passes.Add(1)
	go func() {
		defer s.passes.Done()
		var err error
		for attempt := 0; attempt < revocationPassAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(revocationPassBackoff[min(attempt-1, len(revocationPassBackoff)-1)])
			}
			pctx, cancel := context.WithTimeout(parent, revocationTimeout)
			_, err = pass(pctx)
			cancel()
			if err == nil {
				return
			}
		}
		if s.logger != nil {
			s.logger.Error("auth: the revocation pass over %s failed %d times (last: %v); its record refuses those sessions, and the next revocation runs the pass again", what, revocationPassAttempts, err)
		}
	}()
}

// WaitRevocationPasses blocks until every detached revocation pass started
// so far has returned. No answer depends on a pass: what waits for one reads
// the session rows themselves.
func (s *Service) WaitRevocationPasses() { s.passes.Wait() }

// credentialChangeTTL is how long a credential change holds its account's
// claim (SessionStore.ClaimCredentialChange); credentialChangeBound bounds
// the work done under it, well inside the TTL, so a holder never outlives its
// claim; credentialChangeWait is how long a change waits for another change
// of the same account to release it.
const (
	credentialChangeTTL   = time.Minute
	credentialChangeBound = 40 * time.Second
	credentialChangeWait  = 20 * time.Second
	credentialChangePoll  = 100 * time.Millisecond
)

// claimCredentialChange takes the account's credential-change claim, waiting
// up to credentialChangeWait for another change of the account to release
// it. It returns the context the change runs under — bounded by
// credentialChangeBound — and the release the caller defers. A claim that
// cannot be read is returned as the failure it is.
func (s *Service) claimCredentialChange(ctx context.Context, userID string) (context.Context, func(), error) {
	owner := uuid.NewString()
	deadline := time.Now().Add(credentialChangeWait)
	for {
		cctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
		claimed, err := s.sessions.ClaimCredentialChange(cctx, userID, owner, credentialChangeTTL)
		cancel()
		if err != nil {
			return nil, nil, fmt.Errorf("auth: claim the credential change of user %s: %w", userID, err)
		}
		if claimed {
			break
		}
		if time.Now().After(deadline) {
			return nil, nil, ErrCredentialChangeBusy
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(credentialChangePoll):
		}
	}
	bounded, cancel := context.WithTimeout(ctx, credentialChangeBound)
	release := func() {
		cancel()
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), sessionOpTimeout)
		defer rcancel()
		if err := s.sessions.ReleaseCredentialChange(rctx, userID, owner); err != nil && s.logger != nil {
			s.logger.Warn("auth: release the credential change of user %s: %v (it expires within %s)", userID, err, credentialChangeTTL)
		}
	}
	return bounded, release, nil
}
