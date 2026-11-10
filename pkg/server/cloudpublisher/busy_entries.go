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
// never consented to the fallback door. An absence wins over a busy state the
// same entry is also in (waitsOn): the broker reports a cooldown or closed
// sharing hours before the pledge's bot filter, and opens the credential only
// once the entry's ledger admits — an entry that would refuse this launch once
// free is no wait, whatever it is busy with now.
//
// And a pool that lends one credential per run — the legacy pool, whose
// broker grants the first want that serves — funds at most one provider
// however many of its busy entries free: a refusal naming two or more
// providers that only its busy entries hold does not wait (busyFor's
// oneGrant, UnfundedProvidersError.OneGrant).

// busyEntry is one busy pool entry, named for the sentence.
type busyEntry struct {
	name string
	// oneGrant marks an entry of a pool that lends one credential per run.
	oneGrant bool
}

// busyProviders maps a provider (lowercased) to the busy pool entries the
// walk — or the preview — found for it.
type busyProviders map[string][]busyEntry

// note records one busy entry for prov, once.
func (b busyProviders) note(prov string, entry busyEntry) {
	if b == nil || prov == "" {
		return
	}
	prov = strings.ToLower(prov)
	for _, e := range b[prov] {
		if e.name == entry.name {
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

// waitsOn is the busy reading of one entry a pool passed over, the walk's and
// the preview's: a wait when the entry would serve this launch once free. Its
// state is judged by the verdict no clock clears first (refuses: the pledge
// paused, unhealthy, its bot filter refusing the launch) and must then be a
// busy one (entryBusy); its credential must still be one the walk would open
// (credpool.Broker.CredentialGone, openCredential's own predicate); a lent
// API key's row must serve the launch's audiences — the grant's check
// (acquireLent) would refuse any other. Whatever cannot be read is not proven
// to serve the launch and is no wait.
func (p *Publisher) waitsOn(ctx context.Context, pledgeID string, source credpool.CredentialSource, status, refuses credpool.Status, botID string) bool {
	if refuses != "" {
		status = refuses
	}
	if !entryBusy(status) {
		return false
	}
	if gone, err := p.credPool.CredentialGone(ctx, pledgeID); err != nil || gone != "" {
		return false
	}
	if source == credpool.SourceAPIKey {
		if _, _, cause := p.lentKeyAudience(ctx, pledgeID, "", botID); cause != "" {
			return false
		}
	}
	return true
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

// noteBusySkips records, from a pool's abstention, every declining entry the
// launch waits on (waitsOn). pool names the pool for the sentence; oneGrant
// marks a pool that lends one credential per run.
func (p *Publisher) noteBusySkips(ctx context.Context, busy busyProviders, pool string, oneGrant bool, nd *credpool.NoDonorError, botID string) {
	if busy == nil || nd == nil {
		return
	}
	for _, s := range nd.Skips {
		if !p.waitsOn(ctx, s.PledgeID, s.Credential.Source, s.Status, s.Refuses, botID) {
			continue
		}
		busy.note(providerOfPoolCredential(s.Credential), busyEntry{name: fmt.Sprintf("%s entry %s (%s)", pool, s.PledgeID, s.Status), oneGrant: oneGrant})
	}
}

// noteBusyCandidates is noteBusySkips for the credential preview: the
// candidates a consulted pool showed, read only where the walk would have
// abstained — the walk notes the skips of an abstention alone. A pool that
// lends one credential per run (oneGrant) abstains when its broker serves no
// want, any other per want (the attached walk's slot). served names the wants
// the broker would have served before the lent rows' audiences (previewLent).
func (p *Publisher) noteBusyCandidates(ctx context.Context, busy busyProviders, pool string, oneGrant bool, candidates []credpool.PreviewCandidate, served map[credpool.Credential]bool, botID string) {
	if busy == nil {
		return
	}
	for _, c := range candidates {
		want := credpool.Credential{Source: c.Source, Ref: c.Ref}
		if (oneGrant && len(served) > 0) || served[want] {
			continue
		}
		if !p.waitsOn(ctx, c.PledgeID, c.Source, c.Status, c.Refuses, botID) {
			continue
		}
		busy.note(providerOfPoolCredential(want), busyEntry{name: fmt.Sprintf("%s entry %s (%s)", pool, c.PledgeID, c.Status), oneGrant: oneGrant})
	}
}

// busyFor names the busy entries of each provider in provs, nil when none
// holds one — a refusal carries only what it names — and oneGrant, the
// providers among them only entries of a pool lending one credential per run
// hold, when there are two or more: that pool funds one of them, whatever
// frees.
func (b busyProviders) busyFor(provs []string) (named map[string][]string, oneGrant []string) {
	for _, prov := range provs {
		entries := b[strings.ToLower(prov)]
		if len(entries) == 0 {
			continue
		}
		if named == nil {
			named = map[string][]string{}
		}
		names := make([]string, 0, len(entries))
		only := true
		for _, e := range entries {
			names = append(names, e.name)
			only = only && e.oneGrant
		}
		sort.Strings(names)
		named[prov] = names
		if only {
			oneGrant = append(oneGrant, prov)
		}
	}
	if len(oneGrant) < 2 {
		oneGrant = nil
	}
	return named, oneGrant
}
