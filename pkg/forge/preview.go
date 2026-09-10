package forge

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// EnablePreview is the read-only projection of what enabling a set of bots on
// a repo will provision: the webhook events to subscribe to, the slash-command
// routes the webhook gains, the per-bot forge-token secret binding, the unioned
// token scopes, and any non-installable bots. Computed without any forge write.
type EnablePreview struct {
	Events    []string          // normalized events (forge: blocks ∪ invocations)
	Commands  map[string]string // command name → bot id
	Binds     map[string]string // bot id → workflow-secret name (forge_token default)
	Scopes    map[string]string // unioned token scopes (from forge: blocks)
	Conflicts []string          // bots that cannot be auto-installed, with a reason
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
	commands := map[string]string{}
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
				commands[inv.Command.Name] = b
			}
		}
	}

	return EnablePreview{
		Events:    unionAllEvents(provisionable, frByBot, invByBot),
		Commands:  commands,
		Binds:     binds,
		Scopes:    UnionScopes(reqs...),
		Conflicts: conflicts,
	}
}
