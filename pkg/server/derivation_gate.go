package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// gateDerivation wraps a handler that runs behind requireAuth with the
// credential-derivation chokepoint (auth.Service.CheckDerivation): the
// request's credential must still stand — its account active, its session
// not revoked, a personal access token's row usable — and its kind must be
// one that may do what mint names, or the route answers the refusal before
// next runs. Every route that changes or removes a credential of the account,
// or how an org's accounts sign in, is registered through it.
func (s *Server) gateDerivation(mint auth.Mint, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if _, err := s.checkDerivation(r.Context(), id, mint); err != nil {
			s.derivationError(w, r, err)
			return
		}
		next(w, r)
	})
}

// platformAuthorityStands asks the chokepoint whether the request's
// credential may still exercise platform authority
// (auth.ExercisePlatformAuthority) and reads the super-admin flag from the
// account as the store has it now, not from the credential: the access token
// of a revoked session, of a disabled or pending account, of an account no
// longer a super-admin, and a revoked personal access token are refused. It
// answers the refusal itself and reports whether the caller may go on. A
// server without authentication has no store to ask: the synthesized (or
// test-injected) identity's own flag decides — dev mode stamps a
// super-admin, so production behaviour is unchanged.
func (s *Server) platformAuthorityStands(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.DisableAuth {
		if id, ok := auth.FromContext(r.Context()); ok {
			if !id.IsSuperAdmin {
				httpError(w, http.StatusForbidden, "super-admin only")
				return false
			}
			return true
		}
		return true
	}
	id, _ := auth.FromContext(r.Context())
	d, err := s.checkDerivation(r.Context(), id, auth.ExercisePlatformAuthority)
	if err != nil {
		s.derivationError(w, r, err)
		return false
	}
	if !d.User.IsSuperAdmin {
		httpError(w, http.StatusForbidden, "super-admin only")
		return false
	}
	return true
}

// revokeSuperAdminClaim re-derives the super-admin claim of an access token
// where the identity is established: the flag a JWT carries was minted with
// it and says nothing about the account now. It answers the identity whose
// IsSuperAdmin is what the credential's standing and the account as the store
// reads it say NOW (the credential stands AND its account is a super-admin):
// a demoted or a revoked super-admin's token keeps ordinary access until its
// expiry and loses platform authority — the admin routes and the team/org
// RBAC escape hatches included — at once. A read that fails is an explicit
// error (the caller answers 5xx), never a silent strip. A refusal (a revoked
// session, a disabled or pending account) strips the flag and lets the
// request go on as the ordinary credential it is: a stateless access token
// persists in everything that is not platform authority, which is the
// documented residual.
func (s *Server) revokeSuperAdminClaim(ctx context.Context, id auth.Identity) (auth.Identity, error, error) {
	u, err := s.authSvc.CheckStanding(ctx, id)
	if err != nil {
		if mapAuthErrorStatus(err) >= http.StatusInternalServerError {
			return auth.Identity{}, nil, fmt.Errorf("read the standing of the super-admin claim: %w", err)
		}
		id.IsSuperAdmin = false
		return id, err, nil
	}
	id.IsSuperAdmin = u.IsSuperAdmin
	return id, nil, nil
}

// superAdminRefusalKey carries the standing refusal that stripped a
// super-admin claim, so a platform-admin route answers the refusal itself
// (401, the reason) rather than a 403 that misstates a revoked session as a
// merely unprivileged one.
type superAdminRefusalKey struct{}

func withSuperAdminRefusal(ctx context.Context, refusal error) context.Context {
	return context.WithValue(ctx, superAdminRefusalKey{}, refusal)
}

// superAdminRefusal answers the standing refusal the identity middleware
// recorded for this request's super-admin claim, or nil.
func superAdminRefusal(ctx context.Context) error {
	refusal, _ := ctx.Value(superAdminRefusalKey{}).(error)
	return refusal
}

// platformAuthority answers the in-handler super-admin gates the way
// requireSuperAdmin does: the identity middleware re-derived the claim at
// the identity (revokeSuperAdminClaim), so a standing refusal it recorded
// is re-stated here (401, the reason) and the re-derived flag decides
// (403). A principal no credential reader built — a handler called from a
// test, the dev-mode stamp — is judged on the flag it carries. The routes
// this gates put it FIRST, so a plain caller reaches its own org/role
// refusal without a derivation round-trip.
func (s *Server) platformAuthority(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.DisableAuth {
		if id, ok := auth.FromContext(r.Context()); ok && !id.IsSuperAdmin {
			httpError(w, http.StatusForbidden, "super-admin only")
			return false
		}
		return true
	}
	if refusal := superAdminRefusal(r.Context()); refusal != nil {
		s.derivationError(w, r, refusal)
		return false
	}
	id, _ := auth.FromContext(r.Context())
	if !id.IsSuperAdmin {
		httpError(w, http.StatusForbidden, "super-admin only")
		return false
	}
	return true
}
