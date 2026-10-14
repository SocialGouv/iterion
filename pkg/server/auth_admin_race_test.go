package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// pauseUserWrite parks the FIRST store write to one user id until the test
// releases it, so a second request can commit inside that window — the
// interleaving a deactivation lands in, between a recovery handler's read
// and its write.
//
// It gates BOTH user writers, the whole-row UpdateUser and the field-scoped
// PatchUser: a witness that watched only one of them would pass the day the
// handler changed seam, proving nothing. What tells a real pass from a
// vacuous one is await — it fails when no write ever parks — and each
// subtest's own in-window assertion, which is readable only once the second
// request's write has landed.
type pauseUserWrite struct {
	identity.Store
	userID  string
	reached chan struct{}
	release chan struct{}
	once    sync.Once

	mu   sync.Mutex
	hits int
}

// armPauseUserWrite wraps the service's store and returns the gate. The
// release is also wired to t.Cleanup so a failed assertion cannot leave the
// parked request hanging the test binary.
func armPauseUserWrite(t *testing.T, svc *auth.Service, userID string) *pauseUserWrite {
	t.Helper()
	p := &pauseUserWrite{
		Store:   svc.Store(),
		userID:  userID,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
	svc.SetStoreForTest(p)
	t.Cleanup(p.resume)
	return p
}

func (p *pauseUserWrite) hold(id string) {
	if id != p.userID {
		return
	}
	p.mu.Lock()
	p.hits++
	first := p.hits == 1
	p.mu.Unlock()
	if !first {
		return
	}
	close(p.reached)
	<-p.release
}

func (p *pauseUserWrite) resume() { p.once.Do(func() { close(p.release) }) }

// await blocks until the watched write is parked. A write that never arrives
// is a broken premise, not a pass.
func (p *pauseUserWrite) await(t *testing.T) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("no store write to user %s arrived: the interleaving never happened", p.userID)
	}
}

func (p *pauseUserWrite) UpdateUser(ctx context.Context, u identity.User) error {
	p.hold(u.ID)
	return p.Store.UpdateUser(ctx, u)
}

func (p *pauseUserWrite) PatchUser(ctx context.Context, id string, patch identity.UserPatch) (identity.User, error) {
	p.hold(id)
	return p.Store.PatchUser(ctx, id, patch)
}

// reply carries a parked request's outcome back to the test goroutine, the
// transport error included: only the test goroutine may fail the test.
type reply struct {
	status int
	body   []byte
	err    error
}

// signInOverTheRoute signs a seeded user in through the real login route and
// returns the refresh token the session cookie carries — the handle a test
// needs to read that session's row later. Only the test goroutine may call
// it: it ends the test on a failure.
func signInOverTheRoute(t *testing.T, p *placementE2E, email, password string) string {
	t.Helper()
	status, body, cookies, err := p.callErr(http.MethodPost, "/api/auth/login", "",
		`{"email":"`+email+`","password":"`+password+`"}`)
	if err != nil || status != http.StatusOK {
		t.Fatalf("sign-in = %d body=%s err=%v, want 200", status, body, err)
	}
	for _, c := range cookies {
		if strings.HasSuffix(c.Name, refreshCookieName) {
			return c.Value
		}
	}
	t.Fatalf("the sign-in minted no refresh session: cookies=%v", cookies)
	return ""
}

// TestAdminResetPasswordLosesToConcurrentDisable drives the orderings of
// the ticket's race over real HTTP, through the real super-admin routes.
//
// What the whole-row write costs, measured in both directions:
//   - the reset parked first — a completed deactivation is silently reverted
//     to pending_password_change and a usable temporary password is handed
//     out for an account an admin just closed;
//   - the deactivation parked first — the disable carries the password hash
//     it read, so it RESTORES the credential the reset had just replaced,
//     and the account an operator believes rotated still answers to the old
//     password once re-enabled;
//   - a console edit that names NO status parked first — the one request
//     shape that carries no precondition, so field scoping is its only
//     protection: a name or privilege change must not carry back the status
//     its read predates.
func TestAdminResetPasswordLosesToConcurrentDisable(t *testing.T) {
	t.Run("the reset must not revive an account disabled under it", func(t *testing.T) {
		p := newPlacementE2E(t)
		ctx := context.Background()
		oldHash, err := auth.HashPassword("old-test-password")
		if err != nil {
			t.Fatal(err)
		}
		seedUser(t, p.s, identity.User{ID: "race1", Email: "race1@example.org", PasswordHash: oldHash})
		gate := armPauseUserWrite(t, p.s.authSvc, "race1")
		root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})

		done := make(chan reply, 1)
		go func() {
			status, body, _, err := p.callErr(http.MethodPost, "/api/admin/users/race1/reset-password", root, "")
			done <- reply{status, body, err}
		}()
		gate.await(t)

		// The deactivation completes and is readable while the reset is parked.
		if status, body := p.call(t, http.MethodPatch, "/api/admin/users/race1", root, `{"status":"disabled"}`); status != http.StatusOK {
			t.Fatalf("disable during the reset window = %d body=%s, want 200", status, body)
		}
		mid, err := p.s.authStore().GetUser(ctx, "race1")
		if err != nil {
			t.Fatal(err)
		}
		if mid.Status != identity.UserStatusDisabled {
			t.Fatalf("premise broken: the disable did not commit, status=%q", mid.Status)
		}

		gate.resume()
		got := <-done
		if got.err != nil {
			t.Fatalf("the parked reset never completed: %v", got.err)
		}
		if got.status != http.StatusUnprocessableEntity {
			t.Errorf("reset that lost the race = %d body=%s, want 422", got.status, got.body)
		}
		var response map[string]any
		if err := json.Unmarshal(got.body, &response); err != nil {
			t.Fatalf("decode reset reply: %v (%s)", err, got.body)
		}
		if _, ok := response["temp_password"]; ok {
			t.Error("a reset that lost the race handed out a temporary password")
		}
		// The refusal has to be actionable: "disabled" alone leaves the
		// operator guessing WHICH account the console refused.
		msg, _ := response["error"].(string)
		if !strings.Contains(msg, "disabled") {
			t.Errorf("refusal must say the account is disabled: %q", msg)
		}
		if !strings.Contains(msg, "race1@example.org") {
			t.Errorf("refusal must name the account it refused: %q", msg)
		}
		after, err := p.s.authStore().GetUser(ctx, "race1")
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("the reset revived a disabled account: status=%q", after.Status)
		}
		if after.PasswordHash != oldHash {
			t.Error("the reset replaced the password of an account disabled under it")
		}
	})

	t.Run("the disable must not restore the password the reset replaced", func(t *testing.T) {
		p := newPlacementE2E(t)
		ctx := context.Background()
		oldHash, err := auth.HashPassword("old-test-password")
		if err != nil {
			t.Fatal(err)
		}
		seedUser(t, p.s, identity.User{ID: "race2", Email: "race2@example.org", PasswordHash: oldHash})
		gate := armPauseUserWrite(t, p.s.authSvc, "race2")
		root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})

		done := make(chan reply, 1)
		go func() {
			status, body, _, err := p.callErr(http.MethodPatch, "/api/admin/users/race2", root, `{"status":"disabled"}`)
			done <- reply{status, body, err}
		}()
		gate.await(t)

		// The reset completes inside the deactivation's window.
		status, body := p.call(t, http.MethodPost, "/api/admin/users/race2/reset-password", root, "")
		if status != http.StatusOK {
			t.Fatalf("reset during the disable window = %d body=%s, want 200", status, body)
		}
		var minted struct {
			TempPassword string `json:"temp_password"`
		}
		if err := json.Unmarshal(body, &minted); err != nil || minted.TempPassword == "" {
			t.Fatalf("reset must mint a temporary password: %v (%s)", err, body)
		}

		gate.resume()
		if got := <-done; got.err != nil || got.status != http.StatusOK {
			t.Fatalf("disable = %d body=%s err=%v, want 200", got.status, got.body, got.err)
		}
		after, err := p.s.authStore().GetUser(ctx, "race2")
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("the reset's status write outlived the deactivation: status=%q", after.Status)
		}
		if after.PasswordHash == oldHash {
			t.Error("the deactivation restored the password hash the reset had replaced")
		}
		if ok, err := auth.VerifyPassword(minted.TempPassword, after.PasswordHash); err != nil || !ok {
			t.Errorf("the deactivation discarded the rotated credential: %v", err)
		}
	})

	t.Run("a console edit naming no status must not revive an account disabled under it", func(t *testing.T) {
		p := newPlacementE2E(t)
		ctx := context.Background()
		seedUser(t, p.s, identity.User{ID: "race3", Email: "race3@example.org"})
		gate := armPauseUserWrite(t, p.s.authSvc, "race3")
		root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})

		// PATCH /api/admin/users/{id} deliberately carries no precondition —
		// the console is the authority on a status, not a reader of one — so
		// nothing but the patch's field list keeps this write off the status.
		done := make(chan reply, 1)
		go func() {
			status, body, _, err := p.callErr(http.MethodPatch, "/api/admin/users/race3", root, `{"name":"Renamed By Admin A"}`)
			done <- reply{status, body, err}
		}()
		gate.await(t)

		if status, body := p.call(t, http.MethodPatch, "/api/admin/users/race3", root, `{"status":"disabled"}`); status != http.StatusOK {
			t.Fatalf("disable during the rename window = %d body=%s, want 200", status, body)
		}

		gate.resume()
		got := <-done
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("the parked rename = %d body=%s err=%v, want 200", got.status, got.body, got.err)
		}
		after, err := p.s.authStore().GetUser(ctx, "race3")
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("the rename revived an account disabled under it: status=%q", after.Status)
		}
		if after.Name != "Renamed By Admin A" {
			t.Errorf("name = %q: the field the request owns must still land", after.Name)
		}
	})
}

// TestAdminDisableRevokesSessionsEvenWhenTheReadIsStale is the deactivation's
// other half: the row lands disabled, and every live refresh session must die
// with it — a 15-minute access token is the most the holder may keep.
//
// The decision cannot come from the row read before the write. Here the
// account reads `disabled`, so a handler asking "did I change the status?"
// answers no; meanwhile the window holds a re-enable and a sign-in, and the
// session that sign-in created outlives the deactivation that follows it.
// /api/auth/refresh cannot witness this — it refuses a disabled user whether
// or not the session was revoked — so the session row is read directly.
func TestAdminDisableRevokesSessionsEvenWhenTheReadIsStale(t *testing.T) {
	p := newPlacementE2E(t)
	ctx := context.Background()
	const password = "stale-read-password-321"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	seedUser(t, p.s, identity.User{
		ID: "race4", Email: "race4@example.org", PasswordHash: hash,
		Status: identity.UserStatusDisabled,
	})
	gate := armPauseUserWrite(t, p.s.authSvc, "race4")
	root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})

	done := make(chan reply, 1)
	go func() {
		status, body, _, err := p.callErr(http.MethodPatch, "/api/admin/users/race4", root, `{"status":"disabled"}`)
		done <- reply{status, body, err}
	}()
	gate.await(t)

	if status, body := p.call(t, http.MethodPatch, "/api/admin/users/race4", root, `{"status":"active"}`); status != http.StatusOK {
		t.Fatalf("re-enable during the window = %d body=%s, want 200", status, body)
	}
	refresh := signInOverTheRoute(t, p, "race4@example.org", password)

	gate.resume()
	got := <-done
	if got.err != nil || got.status != http.StatusOK {
		t.Fatalf("the parked disable = %d body=%s err=%v, want 200", got.status, got.body, got.err)
	}
	after, err := p.s.authStore().GetUser(ctx, "race4")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != identity.UserStatusDisabled {
		t.Fatalf("premise broken: the disable did not land, status=%q", after.Status)
	}
	sess, err := p.sessions.GetSessionByTokenHash(ctx, auth.HashRefreshToken(refresh))
	if err != nil {
		t.Fatalf("read the session the sign-in created: %v", err)
	}
	if sess.RevokedAt == nil {
		t.Error("the landing deactivation left the session minted under it alive")
	}
}

// TestAdminEditThatDoesNotDisableKeepsTheSessionsAlive is the other side of
// the same decision: the console edits that do NOT close the account must
// leave the user signed in on every device. One row per request shape the
// route accepts, the two that name no status included — they are the only
// ones that reach the decision's initial value.
//
// Each row also reads the row back: an edit that keeps the sessions alive
// must still DELIVER the field it names. The handler's field list is the only
// thing that carries it — the 200 is unconditional and the audit entry
// records a privilege change from the REQUEST's shape — so a dropped field
// ships as a success whose audit trail claims a change that never landed.
//
// The forced rotation is deliberate, not an omission: the route blocks the
// next SIGN-IN and redirects it to the rotation flow
// (docs/cloud-admin-guide.md §1.6, witnessed in auth_admin_password_test.go),
// while the two transitions that cut live sessions are the disable above and
// the reset-password route below, which replaces the credential too.
func TestAdminEditThatDoesNotDisableKeepsTheSessionsAlive(t *testing.T) {
	const password = "keep-my-session-123"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// parked is written to the row through the store after the sign-in,
		// so a request whose field already holds the value it names still
		// MOVES it — an assertion on a value the seed provides proves
		// nothing.
		parked identity.UserStatus
		body   string
		// landed re-reads the row after the 200 and reports what the request
		// owns and did not deliver: the route answers 200 either way.
		landed func(t *testing.T, u identity.User)
	}{
		{
			name: "a rename",
			body: `{"name":"Renamed By Admin A"}`,
			landed: func(t *testing.T, u identity.User) {
				if u.Name != "Renamed By Admin A" {
					t.Errorf("name = %q, want %q", u.Name, "Renamed By Admin A")
				}
			},
		},
		{
			name: "a privilege grant",
			body: `{"is_super_admin":true}`,
			landed: func(t *testing.T, u identity.User) {
				if !u.IsSuperAdmin {
					t.Error("is_super_admin = false: the grant the console answered 200 to never landed")
				}
			},
		},
		{
			name:   "a re-enable",
			parked: identity.UserStatusDisabled,
			body:   `{"status":"active"}`,
			landed: func(t *testing.T, u identity.User) {
				if u.Status != identity.UserStatusActive {
					t.Errorf("status = %q, want active", u.Status)
				}
			},
		},
		{
			name: "a forced rotation",
			body: `{"status":"pending_password_change"}`,
			landed: func(t *testing.T, u identity.User) {
				if u.Status != identity.UserStatusPendingPasswordChange {
					t.Errorf("status = %q, want pending_password_change", u.Status)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlacementE2E(t)
			seedUser(t, p.s, identity.User{ID: "keep1", Email: "keep1@example.org", PasswordHash: hash})
			refresh := signInOverTheRoute(t, p, "keep1@example.org", password)
			root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})
			if tc.parked != "" {
				parked := tc.parked
				if _, err := p.s.authStore().PatchUser(t.Context(), "keep1", identity.UserPatch{Status: &parked}); err != nil {
					t.Fatalf("park the account as %q: %v", parked, err)
				}
			}

			if status, body := p.call(t, http.MethodPatch, "/api/admin/users/keep1", root, tc.body); status != http.StatusOK {
				t.Fatalf("%s = %d body=%s, want 200", tc.name, status, body)
			}
			after, err := p.s.authStore().GetUser(t.Context(), "keep1")
			if err != nil {
				t.Fatalf("read the row the edit answered 200 for: %v", err)
			}
			tc.landed(t, after)
			sess, err := p.sessions.GetSessionByTokenHash(t.Context(), auth.HashRefreshToken(refresh))
			if err != nil {
				t.Fatalf("read the session the sign-in created: %v", err)
			}
			if sess.RevokedAt != nil {
				t.Errorf("%s revoked the user's live session at %v", tc.name, *sess.RevokedAt)
			}
		})
	}
}

// TestAdminPasswordResetRevokesTheLiveSessions is the hard variant of the
// forced rotation, and the second half of the revocation class: this route
// replaces the credential, so whoever holds the old one loses every live
// session now rather than at the refresh TTL.
func TestAdminPasswordResetRevokesTheLiveSessions(t *testing.T) {
	const password = "the-old-credential-123"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlacementE2E(t)
	seedUser(t, p.s, identity.User{ID: "reset1", Email: "reset1@example.org", PasswordHash: hash})
	refresh := signInOverTheRoute(t, p, "reset1@example.org", password)
	root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})

	if status, body := p.call(t, http.MethodPost, "/api/admin/users/reset1/reset-password", root, ""); status != http.StatusOK {
		t.Fatalf("reset = %d body=%s, want 200", status, body)
	}
	sess, err := p.sessions.GetSessionByTokenHash(t.Context(), auth.HashRefreshToken(refresh))
	if err != nil {
		t.Fatalf("read the session the sign-in created: %v", err)
	}
	if sess.RevokedAt == nil {
		t.Error("the reset left alive the session minted on the credential it replaced")
	}
}
