package main

import (
	"testing"

	iterauth "github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The declarative bootstrap reconcile is the one user writer in this binary,
// and the deliberate exception in the patch conversion: no precondition, so a
// DISABLED bootstrap admin is re-enabled by a restart — the documented
// recovery an operator who locked themselves out depends on. Each drift is
// named, and only it: a patch carrying a field that did not drift would, for
// instance, re-grant super-admin to an account the console had just demoted.
func TestBootstrapAdminDriftNamesOnlyWhatDrifted(t *testing.T) {
	const declared = "declared-bootstrap-secret-1"
	aligned, err := iterauth.HashPassword(declared)
	if err != nil {
		t.Fatal(err)
	}
	other, err := iterauth.HashPassword("a-password-nobody-declared")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		existing    identity.User
		wantSuper   bool
		wantStatus  identity.UserStatus // "" = the patch must not name it
		wantNewHash bool
	}{
		{
			name:     "aligned: an idempotent restart writes nothing",
			existing: identity.User{IsSuperAdmin: true, Status: identity.UserStatusActive, PasswordHash: aligned},
		},
		{
			name:      "the privilege was revoked",
			existing:  identity.User{IsSuperAdmin: false, Status: identity.UserStatusActive, PasswordHash: aligned},
			wantSuper: true,
		},
		{
			name:       "the account was disabled: the documented re-enable",
			existing:   identity.User{IsSuperAdmin: true, Status: identity.UserStatusDisabled, PasswordHash: aligned},
			wantStatus: identity.UserStatusActive,
		},
		{
			name:        "the stored credential is not the declared one",
			existing:    identity.User{IsSuperAdmin: true, Status: identity.UserStatusActive, PasswordHash: other},
			wantNewHash: true,
		},
		{
			name:        "everything drifted at once",
			existing:    identity.User{IsSuperAdmin: false, Status: identity.UserStatusPendingPasswordChange, PasswordHash: ""},
			wantSuper:   true,
			wantStatus:  identity.UserStatusActive,
			wantNewHash: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := bootstrapAdminDrift(tc.existing, declared)
			if err != nil {
				t.Fatalf("bootstrapAdminDrift: %v", err)
			}
			if len(patch.IfNotStatus) != 0 {
				t.Errorf("the reconcile must carry no precondition, got %v: a disabled admin could no longer recover", patch.IfNotStatus)
			}
			if tc.wantSuper != (patch.IsSuperAdmin != nil) {
				t.Errorf("is_super_admin named = %v, want %v", patch.IsSuperAdmin != nil, tc.wantSuper)
			} else if tc.wantSuper && !*patch.IsSuperAdmin {
				t.Error("the reconcile must grant the privilege, not revoke it")
			}
			if tc.wantStatus == "" {
				if patch.Status != nil {
					t.Errorf("status named as %q, want untouched", *patch.Status)
				}
			} else if patch.Status == nil || *patch.Status != tc.wantStatus {
				t.Errorf("status = %v, want %q", patch.Status, tc.wantStatus)
			}
			if tc.wantNewHash != (patch.PasswordHash != nil) {
				t.Errorf("password_hash named = %v, want %v", patch.PasswordHash != nil, tc.wantNewHash)
			} else if tc.wantNewHash {
				if ok, verr := iterauth.VerifyPassword(declared, *patch.PasswordHash); verr != nil || !ok {
					t.Errorf("the reconciled hash does not answer to the declared secret: %v", verr)
				}
			}
			// Nothing else is this writer's: Name, the defaults, the lockout
			// and the login stamp all belong to other paths.
			if patch.Name != nil || patch.DefaultTeamID != nil || patch.DefaultOrgID != nil ||
				patch.GitHubOrgs != nil || patch.LastLoginAt != nil || patch.Lockout != nil {
				t.Errorf("the reconcile named a field it does not own: %+v", patch)
			}
			if patch.Empty() != (!tc.wantSuper && tc.wantStatus == "" && !tc.wantNewHash) {
				t.Errorf("Empty() = %v for %+v", patch.Empty(), patch)
			}
		})
	}
}
