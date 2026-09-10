package native_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// mustBoard reads a store's config or fails the test — the external-test twin
// of the in-package helper (see boardread_test.go).
func mustBoard(t *testing.T, s native.BoardStore) *native.Board {
	t.Helper()
	b, err := s.Board()
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	return b
}
