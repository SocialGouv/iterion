package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/store"
)

type configShareCtxKey struct{}

// configShareFromContext returns the share resolved by configShareAuth.
func configShareFromContext(ctx context.Context) (*configshare.Share, bool) {
	sh, ok := ctx.Value(configShareCtxKey{}).(*configshare.Share)
	return sh, ok
}

// bearerConfigShareToken reads the iws_ token from the Authorization header
// ONLY. The token is never read from a cookie or query param, so a cross-site
// page can't forge a call (structural CSRF immunity) and the raw token never
// lands in a shared cache or the URL.
func bearerConfigShareToken(r *http.Request) string {
	const p = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// configShareAuth authenticates a config-share request: resolve the share by
// the URL id, constant-time verify the Bearer iws_ token, check it is active,
// then stamp a synthetic KindShare identity + tenant on the ctx (which the
// auth layer refuses every operator RBAC gate). Every failure — unknown id,
// bad token, disabled, revoked, expired — collapses to a UNIFORM 401 so the id
// space and lifecycle aren't probeable.
func (s *Server) configShareAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fail := func() { httpError(w, http.StatusUnauthorized, "invalid_share") }

		id := r.PathValue("id")
		token := bearerConfigShareToken(r)
		if s.configShares == nil || id == "" || !strings.HasPrefix(token, configshare.TokenPrefix) {
			fail()
			return
		}
		sh, err := s.configShares.GetByID(r.Context(), id)
		if err != nil || sh == nil || !configshare.VerifyToken(token, sh.TokenHash) || !sh.Active(time.Now().UTC()) {
			fail()
			return
		}

		actor := "share:" + sh.ID
		ctx := auth.WithIdentity(r.Context(), auth.Identity{
			UserID: actor, TeamID: sh.TenantID, Kind: auth.KindShare,
		})
		ctx = store.WithTenant(ctx, sh.TenantID)
		ctx = context.WithValue(ctx, configShareCtxKey{}, sh)

		s.goSafe("configshare-touch", func() {
			_ = s.configShares.Touch(context.Background(), sh.ID, time.Now().UTC())
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
