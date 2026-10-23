package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// injectOnUserWrite commits a mutation to the stored user the moment a write
// to it arrives — the deterministic form of "another request landed between
// this path's read and its write". Nothing here runs on a second goroutine,
// so the witness cannot depend on an ordering nothing guarantees.
//
// It gates BOTH user writers, the whole-row UpdateUser and the field-scoped
// PatchUser: a double that watched one of them would stop injecting the day
// the path changed seam, and the test would pass having proved nothing —
// which is what assertInjected catches.
type injectOnUserWrite struct {
	identity.Store
	t      *testing.T
	userID string
	mutate func(*identity.User)
	fired  bool
}

func armInjectOnUserWrite(t *testing.T, svc *Service, userID string, mutate func(*identity.User)) *injectOnUserWrite {
	t.Helper()
	d := &injectOnUserWrite{Store: svc.Store(), t: t, userID: userID, mutate: mutate}
	svc.SetStoreForTest(d)
	return d
}

func (d *injectOnUserWrite) inject(id string) {
	if id != d.userID {
		return
	}
	if d.fired {
		return
	}
	d.fired = true
	u, err := d.GetUser(context.Background(), id)
	if err != nil {
		d.t.Fatalf("inject: read %s: %v", id, err)
	}
	d.mutate(&u)
	if err := d.Store.UpdateUser(context.Background(), u); err != nil {
		d.t.Fatalf("inject: write %s: %v", id, err)
	}
}

func (d *injectOnUserWrite) UpdateUser(ctx context.Context, u identity.User) error {
	d.inject(u.ID)
	return d.Store.UpdateUser(ctx, u)
}

func (d *injectOnUserWrite) PatchUser(ctx context.Context, id string, p identity.UserPatch) (identity.User, error) {
	d.inject(id)
	return d.Store.PatchUser(ctx, id, p)
}

func (d *injectOnUserWrite) assertInjected() {
	d.t.Helper()
	if !d.fired {
		d.t.Fatal("no user write arrived: this witness proved nothing")
	}
}

func disableUser(u *identity.User) { u.Status = identity.UserStatusDisabled }

// TestAuthWriteCannotUndoAConcurrentDeactivation covers the authentication
// and recovery paths that read a user, derive a credential or a status from
// that snapshot, and write it back. A deactivation committed in that window
// must survive the write and must not yield a session.
func TestAuthWriteCannotUndoAConcurrentDeactivation(t *testing.T) {
	t.Run("ConfirmPasswordReset must not resurrect the account to active", func(t *testing.T) {
		svc, mailer, store := newResetTestService(t)
		ctx := context.Background()
		if err := svc.RequestPasswordReset(ctx, "user@example.org"); err != nil {
			t.Fatalf("RequestPasswordReset: %v", err)
		}
		msg, ok := mailer.last()
		if !ok {
			t.Fatal("no reset email for a known account")
		}
		token := extractResetToken(t, msg)

		d := armInjectOnUserWrite(t, svc, "u1", disableUser)
		res, err := svc.ConfirmPasswordReset(ctx, token, "new-password-456", "ua", "ip")
		if err == nil {
			t.Errorf("a redeemed reset token revived a disabled account: %+v", res.User)
		}
		if res.AccessToken != "" || res.RefreshToken != "" {
			t.Error("the refused reset issued a session")
		}
		d.assertInjected()
		after, err := store.GetUser(ctx, "u1")
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("status after the refused reset = %q, want disabled", after.Status)
		}
	})

	t.Run("ChangePasswordPending must not resurrect the account to active", func(t *testing.T) {
		svc := newTestService(t, SignupOpen)
		ctx := context.Background()
		if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "temp-pass-123", true, identity.UserStatusPendingPasswordChange); err != nil {
			t.Fatalf("CreateUserAndPersonalTeam: %v", err)
		}
		u, err := svc.Store().GetUserByEmail(ctx, "ops@example.com")
		if err != nil {
			t.Fatal(err)
		}

		d := armInjectOnUserWrite(t, svc, u.ID, disableUser)
		res, err := svc.ChangePasswordPending(ctx, "ops@example.com", "temp-pass-123", "brand-new-pass-1", "ua", "ip")
		if err == nil {
			t.Errorf("the forced rotation revived a disabled account: %+v", res.User)
		}
		if res.AccessToken != "" || res.RefreshToken != "" {
			t.Error("the refused rotation issued a session")
		}
		d.assertInjected()
		after, err := d.GetUser(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("status after the refused rotation = %q, want disabled", after.Status)
		}
	})

	t.Run("Login must not mint a session for an account disabled under it", func(t *testing.T) {
		svc := newTestService(t, SignupOpen)
		ctx := context.Background()
		res, err := svc.Register(ctx, "carol@example.com", "correcthorse", "Carol", "", "ua", "ip")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		d := armInjectOnUserWrite(t, svc, res.User.ID, disableUser)
		got, err := svc.Login(ctx, "carol@example.com", "correcthorse", "ua", "ip")
		if !errors.Is(err, ErrAccountDisabled) {
			t.Errorf("login racing a deactivation = %v, want ErrAccountDisabled", err)
		}
		if got.AccessToken != "" || got.RefreshToken != "" {
			t.Error("the login issued a session for a disabled account")
		}
		d.assertInjected()
	})

	t.Run("ChangePassword must not rotate the credential of a disabled account", func(t *testing.T) {
		svc := newTestService(t, SignupOpen)
		ctx := context.Background()
		res, err := svc.Register(ctx, "dave@example.com", "correcthorse", "Dave", "", "ua", "ip")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		before, err := svc.Store().GetUser(ctx, res.User.ID)
		if err != nil {
			t.Fatal(err)
		}

		d := armInjectOnUserWrite(t, svc, res.User.ID, disableUser)
		got, err := svc.ChangePassword(ctx, res.User.ID, "correcthorse", "another-pass-789", "ua", "ip")
		if err == nil {
			t.Error("a self-service rotation landed on an account disabled under it")
		}
		if got.AccessToken != "" || got.RefreshToken != "" {
			t.Error("the refused rotation issued a session")
		}
		d.assertInjected()
		after, err := d.GetUser(ctx, res.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.PasswordHash != before.PasswordHash {
			t.Error("the refused rotation still replaced the password hash")
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("status after the refused rotation = %q, want disabled", after.Status)
		}
	})
}

// TestSwitchTeamDoesNotRestoreARevokedSuperAdmin is the other half of the
// whole-row hazard: a path that owns one cosmetic field (the default team)
// carried IsSuperAdmin and PasswordHash along in its stale copy, so an
// unrelated click re-granted a platform privilege an admin had just revoked
// AND restored the credential a reset had just rotated. Both fields travel in
// the same stale copy, so the witness revokes and rotates in one injection.
func TestSwitchTeamDoesNotRestoreARevokedSuperAdmin(t *testing.T) {
	svc := newTestService(t, SignupOpen)
	ctx := context.Background()
	res, err := svc.Register(ctx, "erin@example.com", "correcthorse", "Erin", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	u, err := svc.Store().GetUser(ctx, res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	u.IsSuperAdmin = true
	if err := svc.Store().UpdateUser(ctx, u); err != nil {
		t.Fatalf("grant super-admin: %v", err)
	}

	const rotated = "rotated-under-the-switch"
	d := armInjectOnUserWrite(t, svc, u.ID, func(cur *identity.User) {
		cur.IsSuperAdmin = false
		cur.PasswordHash = rotated
	})
	if _, err := svc.SwitchTeam(ctx, Identity{Credential: CredentialAccessToken, UserID: u.ID}, res.ActiveTeamID); err != nil {
		t.Fatalf("SwitchTeam: %v", err)
	}
	d.assertInjected()
	after, err := d.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.IsSuperAdmin {
		t.Error("switching team restored a revoked super-admin flag")
	}
	if after.PasswordHash != rotated {
		t.Errorf("switching team restored the credential rotated under it: %q", after.PasswordHash)
	}
	if after.DefaultTeamID != res.ActiveTeamID {
		t.Errorf("default team = %q, want %q: the field the path owns must still move", after.DefaultTeamID, res.ActiveTeamID)
	}
}

// TestBestEffortUserWritesCannotUndoADeactivation covers the writers that own
// ONE field and carry no precondition on purpose: they may not refuse — a
// wrong password must still answer invalid credentials, a switch must still
// re-issue its JWT — so their field list is the only thing keeping a
// deactivation committed inside their window from being undone.
//
// A wrong password is enough to drive the failed-login counter, with no
// credential of any kind: a field it does not own would be an attacker-paced
// revive-and-restore primitive. SwitchOrg is SwitchTeam's twin one level up,
// and carries the same two dangerous fields in its stale copy.
func TestBestEffortUserWritesCannotUndoADeactivation(t *testing.T) {
	const rotated = "installed-by-the-write-that-won"

	t.Run("a failed sign-in stamps the counter only", func(t *testing.T) {
		svc := newTestService(t, SignupOpen)
		ctx := context.Background()
		res, err := svc.Register(ctx, "brute@example.com", "correcthorse", "Brute", "", "ua", "ip")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		d := armInjectOnUserWrite(t, svc, res.User.ID, func(cur *identity.User) {
			cur.Status = identity.UserStatusDisabled
			cur.PasswordHash = rotated
		})
		got, err := svc.Login(ctx, "brute@example.com", "not-the-password-at-all", "ua", "ip")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("a wrong password = %v, want ErrInvalidCredentials", err)
		}
		if got.AccessToken != "" || got.RefreshToken != "" {
			t.Error("a wrong password issued a session")
		}
		d.assertInjected()
		after, rerr := d.GetUser(ctx, res.User.ID)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if after.Status != identity.UserStatusDisabled {
			t.Errorf("status = %q, want disabled: a wrong password revived the account", after.Status)
		}
		if after.PasswordHash != rotated {
			t.Errorf("the counter write restored the credential the racer installed: hash=%q", after.PasswordHash)
		}
		if after.FailedLogins != 1 {
			t.Errorf("failed_logins = %d, want 1: the field the path owns must still land", after.FailedLogins)
		}
	})

	t.Run("SwitchOrg persists the chosen defaults only", func(t *testing.T) {
		svc := newTestService(t, SignupOpen)
		ctx := context.Background()
		res, err := svc.Register(ctx, "grace@example.com", "correcthorse", "Grace", "", "ua", "ip")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		u, err := svc.Store().GetUser(ctx, res.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		u.IsSuperAdmin = true
		if err := svc.Store().UpdateUser(ctx, u); err != nil {
			t.Fatalf("grant super-admin: %v", err)
		}
		org2, err := svc.CreateOrgFor(ctx, u.ID, "Acme Corp", "")
		if err != nil {
			t.Fatalf("CreateOrgFor: %v", err)
		}

		d := armInjectOnUserWrite(t, svc, u.ID, func(cur *identity.User) {
			cur.IsSuperAdmin = false
			cur.PasswordHash = rotated
		})
		if _, err := svc.SwitchOrg(ctx, Identity{Credential: CredentialAccessToken, UserID: u.ID}, org2.ID); err != nil {
			t.Fatalf("SwitchOrg: %v", err)
		}
		d.assertInjected()
		after, rerr := d.GetUser(ctx, u.ID)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if after.IsSuperAdmin {
			t.Error("switching org restored a revoked super-admin flag")
		}
		if after.PasswordHash != rotated {
			t.Errorf("switching org restored the credential rotated under it: %q", after.PasswordHash)
		}
		if after.DefaultOrgID != org2.ID {
			t.Errorf("default org = %q, want %q: the field the path owns must still move", after.DefaultOrgID, org2.ID)
		}
	})
}

// TestLoginWritesRefuseEveryStatusTheirReadRefused is the class the ticket's
// second half lives in: an authentication or recovery path checks a status,
// spends argon2id cycles — 100-400 ms, which a caller streaming attempts can
// pace — and only then writes. Its write must refuse EVERY status its read
// refused, or the path acts on a snapshot an admin moved in between: a login
// whose row flipped to pending_password_change under it would mint a session
// on the hash an admin's reset had just installed, and a self-service
// rotation would replace that temporary password with one the admin never
// issued.
//
// One row per (entry point, refused status) the subtests above leave
// unwitnessed: the three preconditions that name more than `disabled`, and
// the four SSO entry points whose refusal nothing drove.
func TestLoginWritesRefuseEveryStatusTheirReadRefused(t *testing.T) {
	const racerHash = "installed-by-the-write-that-won"
	cases := []struct {
		name string
		// racer is the status a concurrent admin write commits inside the
		// window — one this entry point's own read already refuses.
		racer identity.UserStatus
		// wantErr is the refusal the path translates the store's
		// ErrUserStatusChanged into: the stamp answers ErrAccountDisabled
		// whichever arm refused it, and the two credential rotations stay as
		// opaque as their own wrong-status branch.
		wantErr error
		// arm builds the service, seeds the account, and returns its id plus
		// the call under test. The injection is armed between the two, so
		// only the call's own write is raced.
		arm func(t *testing.T) (*Service, string, func() (LoginResult, error))
	}{
		{
			name:    "Login/pending_password_change",
			racer:   identity.UserStatusPendingPasswordChange,
			wantErr: ErrAccountDisabled,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc := newTestService(t, SignupOpen)
				ctx := context.Background()
				res, err := svc.Register(ctx, "login-raced@example.com", "correcthorse", "Ivan", "", "ua", "ip")
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				return svc, res.User.ID, func() (LoginResult, error) {
					return svc.Login(ctx, "login-raced@example.com", "correcthorse", "ua", "ip")
				}
			},
		},
		{
			name:    "ChangePassword/pending_password_change",
			racer:   identity.UserStatusPendingPasswordChange,
			wantErr: ErrInvalidCredentials,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc := newTestService(t, SignupOpen)
				ctx := context.Background()
				res, err := svc.Register(ctx, "rotate-raced@example.com", "correcthorse", "Judy", "", "ua", "ip")
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				return svc, res.User.ID, func() (LoginResult, error) {
					return svc.ChangePassword(ctx, res.User.ID, "correcthorse", "another-pass-789", "ua", "ip")
				}
			},
		},
		{
			name:    "ChangePasswordPending/active",
			racer:   identity.UserStatusActive,
			wantErr: ErrInvalidCredentials,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc := newTestService(t, SignupOpen)
				ctx := context.Background()
				if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "forced-raced@example.com", "Karl", "temp-pass-123", false, identity.UserStatusPendingPasswordChange); err != nil {
					t.Fatalf("CreateUserAndPersonalTeam: %v", err)
				}
				u, err := svc.Store().GetUserByEmail(ctx, "forced-raced@example.com")
				if err != nil {
					t.Fatal(err)
				}
				return svc, u.ID, func() (LoginResult, error) {
					return svc.ChangePasswordPending(ctx, "forced-raced@example.com", "temp-pass-123", "brand-new-pass-1", "ua", "ip")
				}
			},
		},
		{
			name:    "LoginWithExternal/returning link/disabled",
			racer:   identity.UserStatusDisabled,
			wantErr: ErrAccountDisabled,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc := newTestService(t, SignupOpen)
				ctx := context.Background()
				ext := oidc.ExternalUser{Provider: "google", Subject: "sso-returning", Email: "sso-returning@example.org", Name: "Lena"}
				first, err := svc.LoginWithExternal(ctx, ext, "ua", "ip")
				if err != nil {
					t.Fatalf("first LoginWithExternal: %v", err)
				}
				return svc, first.User.ID, func() (LoginResult, error) {
					return svc.LoginWithExternal(ctx, ext, "ua", "ip")
				}
			},
		},
		{
			name:    "LoginWithExternal/auto-linked email/disabled",
			racer:   identity.UserStatusDisabled,
			wantErr: ErrAccountDisabled,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc := newTestService(t, SignupOpen)
				// The auto-link arm only runs for a provider the operator
				// trusts to verify emails.
				svc.trustedAutoLinkProviders = map[string]struct{}{"google": {}}
				ctx := context.Background()
				if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "sso-linked@example.org", "Mira", "correcthorse", false, identity.UserStatusActive); err != nil {
					t.Fatalf("CreateUserAndPersonalTeam: %v", err)
				}
				u, err := svc.Store().GetUserByEmail(ctx, "sso-linked@example.org")
				if err != nil {
					t.Fatal(err)
				}
				ext := oidc.ExternalUser{Provider: "google", Subject: "sso-new-subject", Email: "sso-linked@example.org"}
				return svc, u.ID, func() (LoginResult, error) {
					return svc.LoginWithExternal(ctx, ext, "ua", "ip")
				}
			},
		},
		{
			name:    "LoginWithExternalForOrg/returning link/disabled",
			racer:   identity.UserStatusDisabled,
			wantErr: ErrAccountDisabled,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc, teamID := orgServiceFixture(t, identity.RoleMember)
				ctx := context.Background()
				ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-returning", Email: "kc-returning@acme.example"}
				first, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
				if err != nil {
					t.Fatalf("first LoginWithExternalForOrg: %v", err)
				}
				return svc, first.User.ID, func() (LoginResult, error) {
					return svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
				}
			},
		},
		{
			name:    "LoginWithExternalForOrg/auto-linked email/disabled",
			racer:   identity.UserStatusDisabled,
			wantErr: ErrAccountDisabled,
			arm: func(t *testing.T) (*Service, string, func() (LoginResult, error)) {
				svc, teamID := orgServiceFixture(t, identity.RoleMember)
				ctx := context.Background()
				domains := enableAutoLink(t, svc)
				now := time.Now()
				if err := domains.Create(ctx, orgsso.VerifiedDomain{
					ID: "d1", TenantID: teamID, Domain: "acme.example", VerifiedAt: &now, CreatedAt: now,
				}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "kc-linked@acme.example", "Nora", "correcthorse", false, identity.UserStatusActive); err != nil {
					t.Fatalf("CreateUserAndPersonalTeam: %v", err)
				}
				u, err := svc.Store().GetUserByEmail(ctx, "kc-linked@acme.example")
				if err != nil {
					t.Fatal(err)
				}
				ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-linked", Email: "kc-linked@acme.example"}
				return svc, u.ID, func() (LoginResult, error) {
					return svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, userID, run := tc.arm(t)
			d := armInjectOnUserWrite(t, svc, userID, func(cur *identity.User) {
				cur.Status = tc.racer
				cur.PasswordHash = racerHash
			})
			got, err := run()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("refusal = %v, want %v (the row was moved to %q): %+v", err, tc.wantErr, tc.racer, got.User)
			}
			if got.AccessToken != "" || got.RefreshToken != "" {
				t.Error("a session was minted on a snapshot that had stopped being true")
			}
			d.assertInjected()
			after, rerr := d.GetUser(context.Background(), userID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if after.Status != tc.racer {
				t.Errorf("status = %q, want %q: the refused write moved it anyway", after.Status, tc.racer)
			}
			if after.PasswordHash != racerHash {
				t.Errorf("the refused write replaced the credential the racer installed: hash=%q", after.PasswordHash)
			}
		})
	}
}

// TestAlsoRefusingKeepsWhatTheCallerNamed pins the shared helper every login
// stamp goes through: the mandatory arm is ADDED, never substituted, listed
// once, and never appended into the caller's own array.
func TestAlsoRefusingKeepsWhatTheCallerNamed(t *testing.T) {
	t.Run("a silent caller gets the mandatory arm", func(t *testing.T) {
		got := alsoRefusing(nil, identity.UserStatusDisabled)
		if len(got) != 1 || got[0] != identity.UserStatusDisabled {
			t.Errorf("got %v, want [disabled]", got)
		}
	})

	t.Run("a caller's arms are kept", func(t *testing.T) {
		got := alsoRefusing([]identity.UserStatus{identity.UserStatusPendingPasswordChange}, identity.UserStatusDisabled)
		if len(got) != 2 || got[0] != identity.UserStatusPendingPasswordChange || got[1] != identity.UserStatusDisabled {
			t.Errorf("got %v, want [pending_password_change disabled]", got)
		}
	})

	t.Run("the mandatory arm is listed once", func(t *testing.T) {
		got := alsoRefusing([]identity.UserStatus{identity.UserStatusDisabled, identity.UserStatusActive}, identity.UserStatusDisabled)
		if len(got) != 2 {
			t.Errorf("got %v, want the two the caller named", got)
		}
	})

	t.Run("the caller's array is never written into", func(t *testing.T) {
		// Spare capacity is where an in-place append would land.
		caller := make([]identity.UserStatus, 1, 4)
		caller[0] = identity.UserStatusActive
		got := alsoRefusing(caller, identity.UserStatusDisabled)
		if len(caller) != 1 || caller[0] != identity.UserStatusActive {
			t.Errorf("the caller's slice changed: %v", caller)
		}
		if len(got) != 2 || got[1] != identity.UserStatusDisabled {
			t.Fatalf("got %v, want [active disabled]", got)
		}
		// The two must not share storage either: a later append to one of
		// them would otherwise rewrite the other's second element.
		got[1] = identity.UserStatusPendingPasswordChange
		if cap(caller) > 1 && caller[:2][1] == identity.UserStatusPendingPasswordChange {
			t.Error("the result aliases the caller's array")
		}
	})
}
