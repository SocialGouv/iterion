package forge

import (
	"context"
	"sort"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

type EnablePreviewCommand struct {
	Command string `json:"command"`
	BotID   string `json:"bot_id"`
}

// EnablePreview is the read-only projection of what enabling a set of bots on
// a repo will provision: the webhook events to subscribe to, the slash-command
// routes the webhook gains, the per-bot forge-token secret binding, the unioned
// token scopes, and any non-installable bots. Computed without any forge write.
type EnablePreview struct {
	Events    []string               // normalized events (forge: blocks ∪ invocations)
	Commands  []EnablePreviewCommand // command routes, including complementary bots
	Binds     map[string]string      // bot id → workflow-secret name (forge_token default)
	Scopes    map[string]string      // unioned token scopes (from forge: blocks)
	Conflicts []string               // bots that cannot be auto-installed, with a reason
	// GatesMerges is the merge-gate posture enabling these bots installs: true
	// when one of them posts a commit status (BotGatesMerges), which is what
	// makes the repo's merges gateable and forces re-review on every push.
	//
	// Computed over the SELECTED bots. Provision derives review_on_sync from
	// the same predicate but over the UNION of the selection with the bots
	// already bound to the repo, so the two agree for a repo not yet
	// integrated on this connection — which is the only case the studio
	// offers the posture for.
	GatesMerges bool
}

// PreviewEnable mirrors Provision's bot-resolution + event/command derivation
// (forge: blocks are optional; a command-only bot subscribes to the comment
// event) so the studio's enable dialog shows exactly what Provision will set
// up — and, crucially, does NOT flag a command-only bot as a conflict. Pure:
// no forge calls, no persistence.
func PreviewEnable(ctx context.Context, teamID string, botFn BotForgeLookup, invFn BotInvocationsLookup, bots []string) EnablePreview {
	frByBot := map[string]*bundle.ForgeRequirements{}
	invByBot := map[string][]bundle.Invocation{}
	binds := map[string]string{}
	commands := []EnablePreviewCommand{}
	var conflicts []string
	var reqs []*bundle.ForgeRequirements
	var provisionable []string

	for _, b := range dedupSorted(bots) {
		fr, err := botFn(ctx, teamID, b)
		if err != nil {
			conflicts = append(conflicts, b+": "+err.Error())
			continue
		}
		var invs []bundle.Invocation
		if invFn != nil {
			invs, _ = invFn(ctx, teamID, b)
		}
		if fr == nil && !hasForgeReachableInvocation(invs) {
			conflicts = append(conflicts, b+": declares neither a forge: block nor a forge/command invocation — not installable")
			continue
		}
		frByBot[b] = fr
		invByBot[b] = invs
		provisionable = append(provisionable, b)

		secret := bundle.DefaultForgeSecretName
		if fr != nil {
			secret = fr.SecretName()
			reqs = append(reqs, fr)
		}
		binds[b] = secret
		for _, inv := range invs {
			if inv.Kind == bundle.InvocationKindCommand && inv.Command != nil {
				commands = append(commands, EnablePreviewCommand{Command: inv.Command.Name, BotID: b})
			}
		}
	}
	sort.Slice(commands, func(i, j int) bool {
		if commands[i].Command == commands[j].Command {
			return commands[i].BotID < commands[j].BotID
		}
		return commands[i].Command < commands[j].Command
	})

	return EnablePreview{
		Events:      unionAllEvents(provisionable, frByBot, invByBot),
		Commands:    commands,
		Binds:       binds,
		Scopes:      UnionScopes(reqs...),
		Conflicts:   conflicts,
		GatesMerges: anyBotGatesMerges(provisionable, frByBot),
	}
}
