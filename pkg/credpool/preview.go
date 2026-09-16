package credpool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// PreviewCandidate describes one attempted donation without materializing it.
// IDs stay internal: a borrowing team may see capacity, not donor identities.
type PreviewCandidate struct {
	PledgeID          string           `json:"-"`
	Source            CredentialSource `json:"source"`
	Ref               string           `json:"ref"`
	Status            Status           `json:"status"`
	Reason            string           `json:"reason,omitempty"`
	Selected          bool             `json:"selected"`
	RemainingUSD      *float64         `json:"remaining_usd,omitempty"`
	LiveRuns          *int             `json:"live_runs,omitempty"`
	MaxConcurrentRuns int              `json:"max_concurrent_runs,omitempty"`
}

// Preview is a point-in-time estimate, never a reservation or proof of unseal.
type Preview struct {
	ObservedAt time.Time          `json:"observed_at"`
	Reason     NoDonorReason      `json:"reason,omitempty"`
	Candidates []PreviewCandidate `json:"candidates"`
}

// Preview follows Acquire's audience, eligibility, preference and fairness
// ordering, using the ledger's own pure admission calculation. It does not
// supersede leases, reserve capacity, mark a donor, probe or open a credential.
// Only new launches are previewed; RunID is deliberately ignored.
func (b *Broker) Preview(ctx context.Context, req Request) (Preview, error) {
	out := Preview{ObservedAt: time.Now().UTC(), Candidates: []PreviewCandidate{}}
	if b == nil {
		out.Reason = ReasonPoolDisabled
		return out, nil
	}
	out.ObservedAt = b.now()
	req.RunID = ""
	if len(req.Wants) == 0 {
		return out, fmt.Errorf("credpool: preview without a credential to look for")
	}
	for _, w := range req.Wants {
		if !w.Source.Valid() || w.Ref == "" {
			return out, fmt.Errorf("credpool: malformed wanted credential %q", w)
		}
	}
	_, pools, err := b.resolvePools(ctx, req)
	if err != nil {
		var nd *NoDonorError
		if errors.As(err, &nd) {
			out.Reason = nd.Reason
			return out, nil
		}
		return out, err
	}
	selected := false
	for _, want := range req.Wants {
		for _, pc := range pools {
			eligible, skips := eligiblePledges(pc.candidates, req, want, out.ObservedAt)
			for _, s := range skips {
				out.Candidates = append(out.Candidates, PreviewCandidate{PledgeID: s.PledgeID, Source: want.Source, Ref: want.Ref, Status: s.Status})
			}
			ranked, err := b.rank(ctx, eligible, out.ObservedAt)
			if err != nil {
				return out, err
			}
			for _, p := range ranked {
				c := PreviewCandidate{PledgeID: p.ID, Source: p.Source, Ref: p.Ref, Status: StatusActive, MaxConcurrentRuns: p.Limits.MaxConcurrentRuns}
				n, committed, err := b.leases.LiveCommitment(ctx, p.ID, "", out.ObservedAt)
				if err != nil {
					c.Status = "unknown"
					c.Reason = "capacity unavailable"
					out.Candidates = append(out.Candidates, c)
					continue
				}
				c.LiveRuns = &n
				day, week, err := b.ledger.Usage(ctx, p.ID, out.ObservedAt)
				if err != nil {
					c.Status = "unknown"
					c.Reason = "usage unavailable"
					out.Candidates = append(out.Candidates, c)
					continue
				}
				live := LiveCommitment{Runs: n, CommittedUSD: committed}
				remaining, deny := previewAdmission(p.Limits, day, week, live)
				if deny != DenyNone {
					c.Status = deny.skipStatus(live)
				} else {
					if p.Limits.MaxUSDPerDay > 0 || p.Limits.MaxUSDPerWeek > 0 {
						c.RemainingUSD = &remaining
					}
					gone, err := b.previewCredential(ctx, p, out.ObservedAt)
					if err != nil {
						c.Status = "unknown"
						c.Reason = "credential metadata unavailable"
					} else if gone != "" {
						c.Status = StatusUnhealthy
						c.Reason = gone
					} else if !selected {
						c.Selected = true
						selected = true
					}
				}
				out.Candidates = append(out.Candidates, c)
			}
		}
	}
	if !selected {
		out.Reason = ReasonNoEligiblePledge
	}
	return out, nil
}

func previewAdmission(l Limits, day, week Usage, live LiveCommitment) (float64, DenyReason) {
	if l.MaxConcurrentRuns > 0 && live.Runs >= l.MaxConcurrentRuns {
		return 0, DenyConcurrency
	}
	return decide(l, day.Runs+1, day.CostUSD, week.CostUSD, live)
}

func oauthCredentialGone(rec secrets.OAuthRecord, now time.Time) string {
	if rec.NotRefreshable && rec.AccessTokenExpiresAt != nil && !now.Before(*rec.AccessTokenExpiresAt) {
		return "the connected subscription expired and carries no refresh token — reconnect it to resume sharing"
	}
	return ""
}
func apiKeyCredentialGone(k secrets.ApiKey, p Pledge, now time.Time) string {
	if k.ScopeUserID == "" || k.ScopeUserID != p.UserID {
		return "that API key is not yours to lend — pledge a personal key instead"
	}
	if k.Provider != secrets.Provider(p.Ref) {
		return "the lent API key no longer matches the pledged provider — pledge it again"
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return "the lent API key has expired — pledge a current one to resume sharing"
	}
	return ""
}
func (b *Broker) previewCredential(ctx context.Context, p Pledge, now time.Time) (string, error) {
	switch p.Source {
	case SourceOAuth:
		rec, err := b.oauth.Get(ctx, p.UserID, secrets.OAuthKind(p.Ref))
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			return "subscription disconnected", nil
		}
		if err != nil {
			return "", err
		}
		return oauthCredentialGone(rec, now), nil
	case SourceAPIKey:
		if b.apiKeys == nil {
			return "", fmt.Errorf("api-key store unavailable")
		}
		k, err := b.apiKeys.GetOwned(ctx, p.KeyID, p.UserID)
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			return "API key deleted", nil
		}
		if err != nil {
			return "", err
		}
		return apiKeyCredentialGone(k, p, now), nil
	}
	return "", fmt.Errorf("unknown credential source")
}
