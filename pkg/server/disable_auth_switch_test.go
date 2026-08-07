package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestDisableAuthSwitchGovernsEveryProtectedRoute locks the ITERION_DISABLE_AUTH
// kill-switch — the single boolean that decides whether /api/* is protected at
// all. `iterion server` reads it from the environment and hands it to
// Config.DisableAuth (cmd/iterion/server.go), and requireAuth branches on it.
//
// It had no test whatsoever, which is the dangerous shape for a switch like
// this: a refactor inverting the sense, or dropping the branch, unauthenticates
// a whole deployment silently and no suite goes red. The two positions are
// asserted here as one contract, because only the PAIR is meaningful — a test
// that pins one position alone passes for a switch welded shut.
func TestDisableAuthSwitchGovernsEveryProtectedRoute(t *testing.T) {
	// A protected route that needs no store wiring to answer: reaching the
	// handler at all is the signal, so any non-401 means auth was bypassed.
	const protected = "/api/backends/detect"

	probe := func(t *testing.T, disableAuth bool) (int, auth.Identity, bool) {
		t.Helper()
		srv := New(Config{DisableAuth: disableAuth}, iterlog.New(iterlog.LevelError, nil))

		// Capture what identity (if any) the middleware injected, so the
		// "authenticated" case is judged on the identity handlers actually
		// see — not merely on a status code.
		var seen auth.Identity
		var had bool
		h := srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, had = auth.FromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, protected, nil))
		return rec.Code, seen, had
	}

	t.Run("off (the default): an anonymous request is refused", func(t *testing.T) {
		code, _, had := probe(t, false)
		if code != http.StatusUnauthorized {
			t.Fatalf("anonymous request = %d, want 401 — the switch is off, so /api/* must be protected", code)
		}
		if had {
			t.Fatal("an identity reached the handler on a refused request")
		}
	})

	t.Run("on: the request is served as a synthetic super-admin", func(t *testing.T) {
		code, id, had := probe(t, true)
		if code != http.StatusOK {
			t.Fatalf("request with the switch on = %d, want 200 — the documented dev-mode bypass", code)
		}
		if !had {
			t.Fatal("no identity injected: handlers relying on one would misbehave")
		}
		// The bypass is only coherent if it synthesises the identity the
		// warning promises; a bypass that injects nothing, or a non-admin,
		// is a different (and worse) behaviour than the documented one.
		if !id.IsSuperAdmin || id.UserID == "" {
			t.Fatalf("dev identity = %+v, want a non-empty super-admin", id)
		}
	})

	t.Run("off: no dev identity leaks into an authenticated-looking request", func(t *testing.T) {
		// A bearer that does not verify must be refused, NOT silently
		// downgraded to the dev identity — the failure mode where the two
		// branches get merged.
		//
		// A real signer is wired here: without one the server answers 500
		// (it cannot verify anything), which would let this case pass for
		// the wrong reason.
		key := make([]byte, 32)
		signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		srv := New(Config{DisableAuth: false, AuthSigner: signer}, iterlog.New(iterlog.LevelError, nil))
		var reached bool
		h := srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, protected, nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bogus bearer = %d, want 401", rec.Code)
		}
		if reached {
			t.Fatal("a request with an invalid bearer reached the handler")
		}
	})
}
