package cloudpublisher

import (
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pairOrderRefusals is ADR-131 §5's second half, in one place: a run replays
// its FROZEN routing pair order as stored — never widened by a default that
// moved since (llmroute.Normalize runs inside Resolve only, so no consumer
// re-normalises a resolved list) — and says ONCE what that list held out.
// ONE record per credential walk, because the walk refuses the same slot at
// the tenant fill, at every shared tier and again at the restore: eight
// copies of one fact is a line an operator learns to skip.
//
// Only the frozen list's OWN refusals: a slot the providers allowlist
// excluded is THAT list's to name, and what names it is partial — the prune
// line at the head of the walk (ADR-129 §5) speaks where a resolved route
// targets the provider, warnRefusedPins where a webhook pinned the key, and
// a credential no route and no pin asks for is held out with no line, a
// silence ADR-129's own clause owns. Two lists refusing one slot must not
// read as two refusals of the same kind: a slot BOTH lists refuse IS this
// list's refusal and stays here — it is the only statement such a slot gets
// — with the allowlist named in the remedy, because the knob that lifts it
// is the other one.
type pairOrderRefusals struct {
	// pairOrder is the list this warning names as the refuser — the launch's
	// resolved one on a launch, the run document's frozen one on a resume.
	pairOrder []string
	// source is the level that won pair_order (llmroute.Source*), read from
	// the policy's own provenance map. Empty when the snapshot carries none:
	// said as unrecorded, never guessed.
	source string
	// slots is the set of refused slots the walk actually HELD a credential
	// for. A slot refused before any store read holds nothing to speak of,
	// and announcing it would fire on every narrowed order forever.
	slots map[string]bool
	// allowed is the run's frozen providers allowlist (ADR-129 §5), nil when
	// none is in force. Kept because this line prescribes a REMEDY, and a
	// remedy names a knob: a slot BOTH frozen lists hold out is the pair
	// order's refusal truthfully, and widening the pair order still would not
	// lift it.
	allowed map[string]bool
}

// newPairOrderRefusals opens the record for one credential walk — on the
// SAME condition that installs the gate, so every refusal the gate hands out
// has a record to land in. A nil policy is an unwired launch: no gate is
// installed, every slot is admitted, and nil is the whole posture (every
// method is nil-safe). A policy whose frozen list is EMPTY is the opposite
// posture — llmroute.SlotSealable refuses every slot — and the run that held
// everything out is the one that most owes the line.
//
// allowed is the walk's providers allowlist, so the remedy can name the knob
// that actually lifts each slot.
func newPairOrderRefusals(policy *store.RunLLMRoutePolicy, allowed map[string]bool) *pairOrderRefusals {
	if policy == nil {
		return nil
	}
	return &pairOrderRefusals{
		pairOrder: policy.PairOrder,
		source:    policy.Sources[llmroute.FieldPairOrder],
		slots:     map[string]bool{},
		allowed:   allowed,
	}
}

// refuses answers, for this run's FROZEN pair order alone, whether it names
// no pair for the slot. The record owns that list, so every message that
// speaks of it asks HERE — the note below, and the pin's remedy, which must
// name this list when the providers allowlist is the one it reports. Nil-safe:
// an unwired launch froze no list and refuses nothing.
func (r *pairOrderRefusals) refuses(slot string) bool {
	return r != nil && !llmroute.SlotSealable(r.pairOrder, slot)
}

// note records a slot the walk HELD a credential for and the run's frozen
// pair order refuses. Silent for a slot the pair order admits: another gate
// (the provider allowlist) refused that one and owns its own line, so the
// attribution does not depend on the order the gates compose in.
func (r *pairOrderRefusals) note(slot string) {
	if !r.refuses(slot) {
		return
	}
	r.slots[slot] = true
}

// alsoRefusedByThePairOrder is the clause a message reporting the PROVIDERS
// ALLOWLIST's refusal owes when the run's frozen pair order refuses the slot
// too — the mirror of alsoHeldByTheAllowlist, for the same reason: a remedy
// that names one of two refusing lists sends the operator to widen it,
// relaunch, and meet the other. Empty when the frozen list admits the slot,
// or when there is no frozen list at all.
func (r *pairOrderRefusals) alsoRefusedByThePairOrder(slot string) string {
	if !r.refuses(slot) {
		return ""
	}
	return ". This run's frozen routing pair order names no pair for the " + slot +
		" slot either: widening the providers list alone does not lift it (ADR-131 §5)"
}

// warn says once, at the end of the walk, which credentials the run's frozen
// pair order held out and where that list came from. Silent when nothing was
// held out — which is every run under the shipped default, whose entries
// cover every sealable slot (llmroute's
// TestDefaultPairOrderCoversEverySealableSlot): the line means something when
// it appears.
//
// The LEVEL follows the provenance, as the ADR-129 §5 prune line does for the
// provider a tenant's own list removed: §5's subject is a list NO level chose
// — a launch day's default a later upgrade may have widened, or a snapshot
// carrying no provenance — and that is the warning. A list a named level WON
// is the operator's own supported narrowing (ADR-121 §1): it is said on every
// launch and every resume of that configuration, so a warn would also be a
// breadcrumb per launch in the tracker (pkg/errtrack's warn+ hook) about a
// list nobody got wrong.
//
// This record's own structural peer disagrees, and the difference is what the
// operator can conclude from the line. audienceWithholdings — the record this
// one is modelled on, deferred from the same walk for the same reason — warns
// per key, on every launch of the configuration that scopes it: an audience
// naming no bot of this lane is a scoping the launch probably did not mean. A
// pair order a level narrowed holds out every OTHER slot the tenant keeps a
// credential for BY CONSTRUCTION, on every launch, which is the configuration
// working: aggregated, and said at the prune line's level.
//
// credentialOwed is the other half of that judgement, and it outranks the
// provenance: a narrowing that left the run with NO LLM credential
// (llmCredentialOwed) is explained by this line alone — the "no credential
// resolved" warning beside it names the tiers and not the list — so saying
// the cause below the symptom's level hands a deployment reading warn+ (a
// supported ITERION_LOG_LEVEL, and pkg/errtrack's hook) the symptom with no
// explanation at all. A FUNDED narrowing is the configuration working and
// stays at the prune line's level; §5's own subject and a narrowing that
// cost the run its credentials are the warn.
func (r *pairOrderRefusals) warn(p *Publisher, runID string, credentialOwed bool) {
	if r == nil || len(r.slots) == 0 || p == nil || p.logger == nil {
		return
	}
	slots := make([]string, 0, len(r.slots))
	for slot := range r.slots {
		slots = append(slots, slot)
	}
	// Sorted: the fact travels with the line, never with map iteration order.
	sort.Strings(slots)
	const line = "cloudpublisher: run=%s — credential(s) resolved for %s held OUT by this run's frozen routing pair order: outside the run's routing pair order, the walk seals no credential for them (ADR-131 §5 — the frozen list is replayed as stored, never widened). %s"
	remedy := r.provenance() + r.alsoHeldByTheAllowlist(slots)
	if r.unchosenList() || credentialOwed {
		p.logger.Warn(line, runID, strings.Join(slots, ", "), remedy)
		return
	}
	p.logger.Info(line, runID, strings.Join(slots, ", "), remedy)
}

// alsoHeldByTheAllowlist names the OTHER frozen list for the slots it refuses
// too, through the one judgement every message prescribing a remedy asks
// (excluderOfSlot). Those slots belong on this line — the pair order does
// refuse them, and this is the only statement a non-pinned slot gets — but
// widening the pair order is not what lifts them, and an operator who follows
// a remedy read off the wrong knob widens one list, seals nothing, and loses
// the line that named the slot. Empty when no allowlist is in force, or when
// it admits every slot named.
func (r *pairOrderRefusals) alsoHeldByTheAllowlist(slots []string) string {
	if r.allowed == nil {
		return ""
	}
	also := make([]string, 0, len(slots))
	for _, slot := range slots {
		if excluderOfSlot(r.allowed, slot) == excludedByProviderAllowlist {
			also = append(also, slot)
		}
	}
	if len(also) == 0 {
		return ""
	}
	return " The providers allowlist (" + model.ProviderAllowlistNames(r.allowed) + ") holds " + strings.Join(also, ", ") +
		" out too: widening pair_order alone does not lift what that list refuses — widen the providers list as well (ADR-129 §5)."
}

// unchosenList reports whether the frozen list is one no level stated — the
// pre-upgrade shapes ADR-131 §5 is about. SourceDefault is the launch day's
// shipped default, which an upgrade since may have widened; an empty source
// is a snapshot that recorded no provenance at all.
func (r *pairOrderRefusals) unchosenList() bool {
	return r.source == "" || r.source == llmroute.SourceDefault
}

// provenance says where the refusing list came from, in terms the operator
// can act on. The shipped default is the LAUNCH DAY's default, which is what
// makes a stored list pre-upgrade: a slot that default did not name then
// stays out of this run however the default reads today, and only a fresh
// launch resolves the current one. A named level is the tenant's own
// statement, and the same bound holds there — a resume hands the run
// document's list back to the walk (ADR-131 §5), so widening the level lifts
// the slot for the NEXT launch, never for the run reading this line; a
// remedy that omits it sends the operator to widen a list this run will not
// re-read. An absent provenance map degrades to "not recorded" rather than
// claiming a level.
func (r *pairOrderRefusals) provenance() string {
	switch r.source {
	case "":
		return "The list's provenance is not recorded on this run (no pair_order source): read llm_route_policy.pair_order on the run document."
	case llmroute.SourceDefault:
		return "The list is the SHIPPED DEFAULT of this run's launch day (pair_order source=default), so a slot that default did not name then stays out of this run however the default reads today: a fresh launch resolves the current one."
	default:
		return "The list was won by pair_order source=" + r.source + ", and that level's own list omits the slot(s): widen it there, or launch under a level that names them — this run replays the list it stored, so only a fresh launch picks the change up."
	}
}
