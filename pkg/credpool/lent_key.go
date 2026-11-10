package credpool

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// lentKeyRow reads the API key row a pledge lends, by the scope the pledge
// names: an operator-pushed entry (LendScope set) reads its reserved scope's
// row, a personal pledge its donor's own. The same rule the grant's opening
// reads it by.
func (b *Broker) lentKeyRow(ctx context.Context, p Pledge) (secrets.ApiKey, error) {
	if b.apiKeys == nil {
		return secrets.ApiKey{}, fmt.Errorf("credpool: no api-key store wired; cannot read %s", p.Credential)
	}
	if p.LendScope != "" {
		return b.apiKeys.GetForLend(ctx, p.KeyID, p.LendScope)
	}
	return b.apiKeys.GetOwned(ctx, p.KeyID, p.UserID)
}

// LentKeyRow returns the API key row the pledge pledgeID lends — the row a
// Grant's KeyID and a PreviewCandidate's PledgeID name — so the borrower can
// hold the row to the audiences it carries (secrets.ApiKey.Bots and .Repos):
// the broker's own selection applies the pledge's bot filter, never the
// row's, and a Request names no repository.
func (b *Broker) LentKeyRow(ctx context.Context, pledgeID string) (secrets.ApiKey, error) {
	if b == nil {
		return secrets.ApiKey{}, fmt.Errorf("credpool: no broker")
	}
	p, err := b.pledges.Get(ctx, pledgeID)
	if err != nil {
		return secrets.ApiKey{}, err
	}
	if p.Source != SourceAPIKey {
		return secrets.ApiKey{}, fmt.Errorf("credpool: pledge %s lends %s, not an API key", pledgeID, p.Credential)
	}
	return b.lentKeyRow(ctx, p)
}
