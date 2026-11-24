package server

import "github.com/SocialGouv/iterion/pkg/webhooks"

// applyCardIntegrationVarLayers hands a carded run the var layers of the
// integration its card names (#2665 item 1, ARB-7): read LIVE by the carried
// id — exactly as cardLaunchPolicyLayers reads the routing block — and
// layered around the card's own bot_args in the one order every lane now
// shares: `launch_vars` UNDER the bot_args, `operator_launch_vars` OVER
// them, the bot's declared defaults below all three. The operator layer is
// the operator's own gesture, so its names are the lane's trusted names
// (ADR-129 §4 rule 2, the parity with the direct lane) and a bot_arg that
// carries one of them with another value is IGNORED and said so through
// warn: a member-writable, payload-derived value never overrides the
// operator's, and an admin narrowing the integration reaches the cards
// already sitting in its columns.
func applyCardIntegrationVarLayers(botArgs map[string]string, cfg webhooks.Config, warn func(format string, args ...any)) (vars map[string]string, trusted []string) {
	vars = make(map[string]string, len(cfg.LaunchVars)+len(botArgs)+len(cfg.OperatorLaunchVars))
	mergeVarsInto(vars, cfg.LaunchVars)
	mergeVarsInto(vars, botArgs)
	for name, operator := range cfg.OperatorLaunchVars {
		if carried, ok := botArgs[name]; ok && carried != operator && warn != nil {
			warn("card bot_arg %q = %q is ignored: integration %s pins it to %q on its operator layer (a member-writable value never overrides the operator's — ADR-129 §4 rule 2)", name, carried, cfg.ID, operator)
		}
		vars[name] = operator
	}
	return vars, trustedVarNames(cfg.OperatorLaunchVars)
}
