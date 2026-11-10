package cloudpublisher

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A lent API key keeps its own audiences: the pledge's and the key row's
// intersect (docs/byok.md). secrets.Resolve treats a key row's Bots and Repos as a GATE — no
// pin and no restore hands the key to a bot or a repository its operator did
// not name — but the broker opens a lent row by id and checks ownership,
// provider and expiry only, and a credpool.Request names no repository at
// all. So every grant of a lent key passes HERE before the walk may seal it —
// acquireLent is the one Acquire the legacy pool, the attached pool and the
// fallback door call — and the preview's twin (previewLent) applies the same
// reading to every candidate it shows.

// LentKeyWithheldError is the abstention of a grant whose lent key row's
// audience excludes the launch, or whose row could not be read to check it:
// the grant was released, nothing sealed. It unwraps to credpool.ErrNoDonor
// — an abstention, like every other way a pool serves nothing.
type LentKeyWithheldError struct {
	// KeyID and Name identify the lent key row (Name is empty when the row
	// could not be read).
	KeyID, Name string
	// Cause says which audience refused, or why the row could not be read.
	Cause string
}

func (e *LentKeyWithheldError) Error() string {
	return fmt.Sprintf("credential pool grant released: the lent key %q (%s) %s", e.Name, e.KeyID, e.Cause)
}

func (e *LentKeyWithheldError) Unwrap() error { return credpool.ErrNoDonor }

// lentKeyAudience judges the row the pledge lends against the launch: the
// canonical bot the BYOK audience compares (botregistry.NormalizeName of the
// request's bot, the walk's audienceBotID) and the repository stamped on ctx
// (secrets.WithLaunchRepo). keyID is the row the grant opened, when known: a
// pledge re-pointed at another row since is not the key that was granted. ""
// cause when the row serves the launch; otherwise the cause in the walk's own
// words (audienceWithholdings), and why names the audience that refused (both
// false for a row that could not be read — not proven to serve the launch).
func (p *Publisher) lentKeyAudience(ctx context.Context, pledgeID, keyID, botID string) (k secrets.ApiKey, why secrets.AudienceRefusal, cause string) {
	k, err := p.credPool.LentKeyRow(ctx, pledgeID)
	switch {
	case err != nil:
		return k, why, fmt.Sprintf("could not be read to check its audience (%v)", err)
	case keyID != "" && k.ID != keyID:
		return k, why, fmt.Sprintf("is no longer the row its pledge lends (the pledge names %s now)", k.ID)
	}
	audienceBotID := botregistry.NormalizeName(botID)
	repo := secrets.LaunchRepoFromContext(ctx)
	why = secrets.AudienceRefusal{Bot: !k.ServesBotForLaunch(audienceBotID), Repo: !k.ServesRepoForLaunch(repo)}
	if !why.Bot && !why.Repo {
		return k, why, ""
	}
	said := newAudienceWithholdings(audienceBotID, repo)
	said.note(k, why)
	return k, why, "is withheld from this launch: " + said.cause(k.ID)
}

// acquireLent is the one Acquire of the walk's pool paths: the broker's grant,
// held to the lent key row's own audience. A grant the audience refuses is
// released here and handed back as released — the acquisition superseded a
// lease all the same, so the caller's rollback must reopen it — with a
// *LentKeyWithheldError. A subscription has no key row and passes as granted.
func (p *Publisher) acquireLent(ctx context.Context, req credpool.Request) (granted, released *credpool.Grant, err error) {
	grant, err := p.credPool.Acquire(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if grant.Source != credpool.SourceAPIKey {
		return grant, nil, nil
	}
	k, _, cause := p.lentKeyAudience(ctx, grant.PledgeID, grant.KeyID, req.BotID)
	if cause == "" {
		return grant, nil, nil
	}
	p.credPool.ReleaseGrant(ctx, grant)
	return nil, grant, &LentKeyWithheldError{KeyID: grant.KeyID, Name: k.Name, Cause: cause}
}

// previewLent is acquireLent's twin for the read-only preview: the broker's
// Preview, every lent-key candidate judged by the same reading. A candidate
// its row's audience refuses is shown in the state apiStage gives a key its
// own audience keeps out (bot_filtered, else repo_filtered) — the row's lists
// stay out of the reason, as apiStage keeps a shared key's out — and when it
// was the selection the preview selects nothing in its place: the live grant
// is released, and the broker serves no second pledge for that want.
func (p *Publisher) previewLent(ctx context.Context, req credpool.Request) (credpool.Preview, error) {
	out, err := p.credPool.Preview(ctx, req)
	if err != nil {
		return out, err
	}
	audienceBotID := botregistry.NormalizeName(req.BotID)
	for i := range out.Candidates {
		c := &out.Candidates[i]
		if c.Source != credpool.SourceAPIKey || c.PledgeID == "" {
			continue
		}
		_, why, cause := p.lentKeyAudience(ctx, c.PledgeID, "", req.BotID)
		if cause == "" {
			continue
		}
		if c.Selected {
			c.Selected = false
			out.Reason = credpool.ReasonNoEligiblePledge
		}
		switch {
		case why.Bot:
			c.Status = credpool.StatusBotFiltered
			c.Reason = "The lent key's workload audience does not name bot " + audienceBotID + "."
		case why.Repo:
			c.Status = credpool.StatusRepoFiltered
			c.Reason = "The lent key's repository audience does not name " + previewLaunchRepo(secrets.LaunchRepoFromContext(ctx)) + "."
		default:
			c.Status = "unknown"
			c.Reason = "The lent key's audience could not be read."
		}
	}
	return out, nil
}
