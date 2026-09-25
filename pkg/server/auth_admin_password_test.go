package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

func TestAdminResetPasswordDisabledAccount(t *testing.T) {
	for _, passwordLogin := range []bool{false, true} {
		name := "sso-only"
		if passwordLogin {
			name = "password"
		}
		t.Run(name, func(t *testing.T) {
			p := newPlacementE2E(t)
			ctx := context.Background()
			u := identity.User{
				ID: "disabled", Email: "disabled@example.org", Status: identity.UserStatusDisabled,
				Name: "Disabled account", UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			}
			if passwordLogin {
				var err error
				u.PasswordHash, err = auth.HashPassword("old-test-password")
				if err != nil {
					t.Fatal(err)
				}
			}
			u, err := p.s.authStore().CreateUser(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})
			path := "/api/admin/users/disabled/reset-password"
			status, body := p.call(t, http.MethodPost, path, root, "")
			if status != http.StatusUnprocessableEntity {
				t.Errorf("disabled reset status = %d, want 422", status)
			}
			var response map[string]any
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			message, _ := response["error"].(string)
			for _, want := range []string{u.Email, "disabled", "re-enable"} {
				if !strings.Contains(message, want) {
					t.Errorf("refusal must name %q: %q", want, message)
				}
			}
			if _, ok := response["temp_password"]; ok {
				t.Error("refused reset returned a temporary password")
			}
			after, err := p.s.authStore().GetUser(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, u) {
				t.Error("refused reset changed the stored account")
			}
			if _, err := p.s.authSvc.Login(ctx, u.Email, "old-test-password", "", ""); !errors.Is(err, auth.ErrAccountDisabled) {
				t.Errorf("login after refusal = %v, want account disabled", err)
			}

			// Recovery remains available after an explicit, separate re-enable.
			status, _ = p.call(t, http.MethodPatch, "/api/admin/users/disabled", root, `{"status":"active"}`)
			if status != http.StatusOK {
				t.Fatalf("explicit re-enable status = %d", status)
			}
			status, body = p.call(t, http.MethodPost, path, root, "")
			assertAdminPasswordReset(t, p, u, status, body)
		})
	}
}

func TestAdminResetPasswordEnabledAccounts(t *testing.T) {
	for _, state := range []identity.UserStatus{identity.UserStatusActive, identity.UserStatusPendingPasswordChange} {
		t.Run(string(state), func(t *testing.T) {
			p := newPlacementE2E(t)
			u, err := p.s.authStore().CreateUser(context.Background(), identity.User{
				ID: "reset", Email: "reset@example.org", Status: state, PasswordHash: "old-test-hash",
			})
			if err != nil {
				t.Fatal(err)
			}
			root := p.jwt(t, auth.Identity{UserID: "root", IsSuperAdmin: true})
			status, body := p.call(t, http.MethodPost, "/api/admin/users/reset/reset-password", root, "")
			assertAdminPasswordReset(t, p, u, status, body)
		})
	}
}

func assertAdminPasswordReset(t *testing.T, p *placementE2E, before identity.User, status int, body []byte) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("enabled reset status = %d, want 200", status)
	}
	var response struct {
		TempPassword string `json:"temp_password"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.TempPassword == "" {
		t.Fatalf("reset response must contain a temporary password: %v", err)
	}
	after, err := p.s.authStore().GetUser(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != identity.UserStatusPendingPasswordChange || after.PasswordHash == before.PasswordHash {
		t.Error("reset did not require rotation with a new password hash")
	}
	if ok, err := auth.VerifyPassword(response.TempPassword, after.PasswordHash); err != nil || !ok {
		t.Errorf("returned temporary password does not match stored hash: %v", err)
	}
	if _, err := p.s.authSvc.Login(context.Background(), after.Email, response.TempPassword, "", ""); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Errorf("temporary password login = %v, want forced rotation", err)
	}
}
