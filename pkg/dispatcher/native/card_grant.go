package native

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The publish grant a server mints for one run never belongs on a board card:
// the mint happens at launch (applyPRLaunchContext), the card-launch lanes drop
// whatever grant a card carries, and a card is a long-lived record a bot, an
// operator and an LLM all read. Two measures answer that, and both read the
// same class list (store.ServerMintedLaunchVars):
//
//   - WRITE: every BoardStore writer — REST, the board MCP tools, `iterion
//     issue`, trigger promote — lands in Create or Update, where
//     store.DropServerMintedVars removes the grant vars a client sent. One
//     decision for the four writers, held to the same bar on both store twins
//     by the BoardStore conformance suite.
//   - READ: a card written before the drop existed can still carry a LIVE
//     bearer (the studio's Edit form used to pre-fill it from the pipeline
//     projection, which showed it in clear), and the grant's TTL outlives the
//     run by days. So every surface that hands a card out — IssueForRead below
//     — masks the bearer instead of printing it.
//
// The mask is deliberately NOT applied inside the store's own reads: the launch
// lane needs the card's real bearer to tell a live grant of ANOTHER team (a
// crossing it refuses, forge_publish.go withoutCardGrant) from the mask a view
// showed (which it drops). A mask under the store would answer "no crossing" to
// every card.

// BotArgsForWrite is bot args as a board may persist them: without the publish
// grant vars a writer sent under the names a server mints
// (store.DropServerMintedVars — an EMPTY value is kept, it withdraws rather
// than grants). Both store twins call it at Create and at Update, which is
// where every writer's intent arrives; args is never mutated.
//
// The deeper write funnel (the FS twin's writeIssueLocked, the Mongo replace)
// is deliberately NOT the site: there, a card's existing args cannot be told
// from args this write supplies, so a state transition would silently scrub a
// pre-existing grant — and the launch lane's crossing refusal reads that
// bearer.
func BotArgsForWrite(args map[string]string) map[string]string {
	return store.DropServerMintedVars(args)
}

// KeepStoredMintedVars is the wholesale-patch half of that same boundary: the
// grant vars a writer SENT are gone (BotArgsForWrite already ran), and the
// ones the STORED card still holds come back off it, so an args rewrite — a
// set_bot re-point, a studio Edit form, a trigger promote — scrubs no live
// grant the launch lane still reads to refuse a crossing. A wholesale patch
// may still retire one: the empty value that withdraws survives
// BotArgsForWrite and names the var in kept, which blocks the re-stamp — the
// patch said what it wanted for that name. The reserved twin of this move is
// KeepStoredLaunchContext (card_launch_args.go).
//
// Called by both store twins at Update, right after BotArgsForWrite — the
// same single site, held to the same bar on both twins by the BoardStore
// conformance suite. Create never calls it: a card being created holds
// nothing to keep. Neither map is mutated.
func KeepStoredMintedVars(stored, kept map[string]string) map[string]string {
	held := false
	for _, name := range store.ServerMintedLaunchVars {
		if v, ok := stored[name]; ok && strings.TrimSpace(v) != "" {
			if _, inKept := kept[name]; !inKept {
				held = true
				break
			}
		}
	}
	if !held {
		return kept
	}
	out := make(map[string]string, len(kept)+len(store.ServerMintedLaunchVars))
	for k, v := range kept {
		out[k] = v
	}
	for _, name := range store.ServerMintedLaunchVars {
		if v, ok := stored[name]; ok && strings.TrimSpace(v) != "" {
			if _, inKept := kept[name]; !inKept {
				out[name] = v
			}
		}
	}
	return out
}

// IssueForRead is iss as a read surface may hand it out: its bot args carry the
// mask (store.RedactedLaunchVar) in place of a publish grant the card still
// holds. Returns iss itself when the card carries no grant, and never mutates
// it — the caller's record may be the store's own clone.
//
// A shallow copy is enough: only BotArgs is replaced, and it is replaced by a
// fresh map rather than edited in place.
func IssueForRead(iss *Issue) *Issue {
	if iss == nil || !store.HasServerMintedSecret(iss.BotArgs) {
		return iss
	}
	out := *iss
	out.BotArgs = store.RedactLaunchStringVars(iss.BotArgs)
	return &out
}

// IssuesForRead is IssueForRead over a list — the board's list surfaces. It
// returns the same slice when no card carries a grant, and never mutates the
// caller's slice or the records in it.
func IssuesForRead(issues []*Issue) []*Issue {
	masked := false
	for _, iss := range issues {
		// A nil entry is a record the caller carries as it is, not a panic
		// to take on a read surface.
		if iss != nil && store.HasServerMintedSecret(iss.BotArgs) {
			masked = true
			break
		}
	}
	if !masked {
		return issues
	}
	out := make([]*Issue, len(issues))
	for i, iss := range issues {
		out[i] = IssueForRead(iss)
	}
	return out
}
