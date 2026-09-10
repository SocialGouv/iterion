package server

import "testing"

// Forgejo/Gitea answer "owner" for the repository owner; every consumer of the
// rank table (command gate, review-request gate, review-thread gate,
// author-trust gate) must place it above every floor a webhook can pin.
func TestPrforgePermRank_OwnerRanksAtTheTop(t *testing.T) {
	got := prforgePermRank("owner")
	if got < replierMinRoleRank("maintainer") || got < prforgePermRank("admin") {
		t.Fatalf("owner ranks %d — a Forgejo/Gitea repo owner must clear every floor (maintainer=%d, admin=%d)", got, replierMinRoleRank("maintainer"), prforgePermRank("admin"))
	}
}
