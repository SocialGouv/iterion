package cloudpublisher

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/credpool"
)

// A pool entry that exists for the launch but cannot serve it NOW is a wait,
// not an absent credential: another run holds its slot (serving), its day's
// runs or allowance — or the attachment's own draw cap — are spent
// (exhausted), its provider window is closed (cooling), it is outside its
// sharing hours (out_of_hours), or its reading sits past the strategy's
// margin (near_limit). Each clears with time, as a team key at its
// max_concurrent_runs clears when one of its runs ends — and the walk
// answers that key with the capacity class (ErrCredentialAtCapacity, the
// admission's refusal), so a refusal whose every named provider only such an
// entry holds answers the same class (UnfundedProvidersError.Busy): the retry
// sweeper asks again on the slot cadence, free of charge, until the run's
// max_wait, an operator is answered 429, and the sentence says the entry is
// busy rather than asking for a credential that exists.
//
// The other pool states are absences a wait does not cure: a paused entry, an
// unhealthy one (its credential failing or gone), one whose pledge's bot
// filter or whose lent row's own audiences refuse the launch, one whose donor
// never consented to the fallback door.

// busyProviders maps a provider (lowercased) to the busy pool entries the
// walk — or the preview — found for it, each named for the sentence.
type busyProviders map[string][]string

// note records one busy entry for prov, once.
func (b busyProviders) note(prov, entry string) {
	if b == nil || prov == "" {
		return
	}
	prov = strings.ToLower(prov)
	for _, e := range b[prov] {
		if e == entry {
			return
		}
	}
	b[prov] = append(b[prov], entry)
}

// entryBusy reports whether a pool entry's state is an availability one — the
// entry exists for the launch and clears with time (serving, exhausted,
// cooling, out of hours, near limit) — rather than an absence.
func entryBusy(st credpool.Status) bool {
	switch st {
	case credpool.StatusServing, credpool.StatusExhausted, credpool.StatusCooling, credpool.StatusOutOfHours, credpool.StatusNearLimit:
		return true
	}
	return false
}

// providerOfPoolCredential is the provider a pool credential funds: an API
// key's provider, a subscription kind's.
func providerOfPoolCredential(c credpool.Credential) string {
	switch c.Source {
	case credpool.SourceAPIKey:
		return strings.ToLower(c.Ref)
	case credpool.SourceOAuth:
		return providerOfOAuthKind(c.Ref)
	}
	return ""
}

// noteBusySkips records, from a pool's abstention, every declining entry that
// exists for the launch and is only unavailable now: a busy state (entryBusy)
// and, for a lent API key, a row whose own audiences serve the launch — the
// grant's audience check (acquireLent) would refuse any other, so waiting on
// it would wait for nothing. A row that cannot be read is not proven to serve
// the launch and is not recorded. pool names the pool for the sentence.
func (p *Publisher) noteBusySkips(ctx context.Context, busy busyProviders, pool string, nd *credpool.NoDonorError, botID string) {
	if busy == nil || nd == nil {
		return
	}
	for _, s := range nd.Skips {
		if !entryBusy(s.Status) {
			continue
		}
		if s.Credential.Source == credpool.SourceAPIKey {
			if _, _, cause := p.lentKeyAudience(ctx, s.PledgeID, "", botID); cause != "" {
				continue
			}
		}
		busy.note(providerOfPoolCredential(s.Credential), fmt.Sprintf("%s entry %s (%s)", pool, s.PledgeID, s.Status))
	}
}

// noteBusyCandidates is noteBusySkips for the credential preview: the
// candidates a consulted pool showed, each already judged by its lent row's
// audiences (previewLent).
func noteBusyCandidates(busy busyProviders, pool string, candidates []credpool.PreviewCandidate) {
	for _, c := range candidates {
		if c.Selected || !entryBusy(c.Status) {
			continue
		}
		busy.note(providerOfPoolCredential(credpool.Credential{Source: c.Source, Ref: c.Ref}), fmt.Sprintf("%s entry %s (%s)", pool, c.PledgeID, c.Status))
	}
}

// busyFor names the busy entries of each provider in provs, nil when none
// holds one — a refusal carries only what it names.
func (b busyProviders) busyFor(provs []string) map[string][]string {
	var out map[string][]string
	for _, prov := range provs {
		entries := b[strings.ToLower(prov)]
		if len(entries) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		sorted := append([]string(nil), entries...)
		sort.Strings(sorted)
		out[prov] = sorted
	}
	return out
}
