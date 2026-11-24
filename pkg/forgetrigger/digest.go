package forgetrigger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// digestFormat versions the canonical document ManifestDigest hashes. A
// change of that document's shape moves it, so the rows projected under the
// previous shape read as stale instead of being routed by.
const digestFormat = 1

// digestDocument is the canonical form of a plan's manifest-side inputs.
// Its slices are in a fixed order (bots by name, routes in manifest order)
// and its maps are encoded with sorted keys, so the same inputs always give
// the same bytes.
type digestDocument struct {
	Format int         `json:"format"`
	Bots   []digestBot `json:"bots"`
	Roles  digestRoles `json:"roles"`
}

type digestRoles struct {
	Implementer string `json:"implementer"`
	Reviewer    string `json:"reviewer"`
}

type digestBot struct {
	ID         string            `json:"id"`
	Routes     []digestRoute     `json:"routes"`
	LaunchVars map[string]string `json:"launch_vars"`
}

// digestRoute is what a row is built from of one
// bundle.ForgeRoutingInvocation: the key its id derives from, and the fields
// it copies.
type digestRoute struct {
	Key         string            `json:"key"`
	Mode        string            `json:"mode"`
	Labels      []string          `json:"labels"`
	ArgsVar     string            `json:"args_var"`
	OpensMR     bool              `json:"opens_mr"`
	ContextVars map[string]string `json:"context_vars"`
}

// DigestBots is the bot set a projection digest hashes, whichever producer
// computes it — the projector when it plans an integration, the router when
// it judges that integration's rows stale: the integration's own bots
// (RepoIntegration.BotIDs), sorted and deduplicated. Never the webhook
// config's bot scope, which an operator may PATCH apart from the integration
// (bot_ids, wildcard_bots) with no projection to follow.
func DigestBots(ri forge.RepoIntegration) []string {
	return sortedBots(ri.BotIDs)
}

// ManifestDigest is the digest of the manifest-side inputs a projection is
// planned from: the bot set, each bot's issue routes
// (bundle.ForgeIssueInvocations) and manifest launch vars, and the role
// bots. PlanIntegration stamps it on every row (Subscription.ProjectionDigest)
// and reports it; a router that recomputes it from the live catalogue, with
// the arguments the projection was given, holds stale rows when the two
// differ.
//
// It is 16 hexadecimal characters, and canonical: the order of bots, a bot
// listed twice and the iteration order of the maps change nothing. A bot
// with no entry in invByBot or frByBot is hashed as a bot that claims
// nothing. The integration's per-bot operator state (BotTriggers) is not an
// input: every write of it projects on the spot.
func ManifestDigest(bots []string, invByBot map[string][]bundle.Invocation, frByBot map[string]*bundle.ForgeRequirements, roles RoleBots) string {
	doc := digestDocument{
		Format: digestFormat,
		Bots:   []digestBot{},
		Roles:  digestRoles(roles),
	}
	for _, bot := range sortedBots(bots) {
		entry := digestBot{ID: bot, Routes: []digestRoute{}, LaunchVars: map[string]string{}}
		maps.Copy(entry.LaunchVars, manifestLaunchVars(frByBot[bot]))
		routes, _ := bundle.ForgeIssueInvocations(frByBot[bot], invByBot[bot])
		for _, route := range routes {
			r := digestRoute{
				Key:         route.Key,
				Mode:        digestMode(route.Inv),
				Labels:      append([]string{}, route.Labels...),
				ArgsVar:     route.ArgsVar,
				OpensMR:     route.OpensMR,
				ContextVars: map[string]string{},
			}
			maps.Copy(r.ContextVars, route.Inv.ContextVars)
			entry.Routes = append(entry.Routes, r)
		}
		doc.Bots = append(doc.Bots, entry)
	}
	// The document holds only strings, booleans, slices and string maps: its
	// encoding cannot fail.
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// digestMode is the mode a route's row runs in, as the row stores it: the
// resolved mode, so an invocation that spells the default hashes like one
// that leaves it out. A mode that does not resolve — no route carries one —
// is hashed as declared, apart from every resolved mode.
func digestMode(inv bundle.Invocation) string {
	mode, err := inv.EffectiveMode()
	if err != nil {
		return "unresolved:" + string(inv.Mode)
	}
	return string(mode)
}
