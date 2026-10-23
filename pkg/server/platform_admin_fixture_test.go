package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// seedPlatformAdmin writes the account a fixture's super-admin token names:
// the platform-admin routes read the super-admin flag from the account as the
// store has it (platformAuthorityStands), never from the token alone.
func seedPlatformAdmin(t *testing.T, st identity.Store, id string) {
	t.Helper()
	if _, err := st.CreateUser(context.Background(), identity.User{
		ID: id, Email: id + "@example.org", Status: identity.UserStatusActive,
		IsSuperAdmin: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed the platform admin %s: %v", id, err)
	}
}
