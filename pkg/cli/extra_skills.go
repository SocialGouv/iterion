package cli

import (
	"os"
	"strings"
)

// EnvExtraSkills is the machine-wide default for run-level skills.
//
// The highest-value form of this feature: an operator with a house authoring
// standard sets it once and every run on that machine carries it, whatever bot
// it launches, with no catalog edit. Comma-separated skill-library names.
const EnvExtraSkills = "ITERION_SKILLS"

// ResolveExtraSkills merges the `--skill` flag with the ITERION_SKILLS machine
// default and reports where the result came from ("flag", "env", or "flag+env"
// — carried onto the skills_injected event so a run says not just WHAT it
// carried but why).
//
// A UNION rather than the usual override chain, because these do not compete:
// a machine-wide standard and a per-run addition are both things the operator
// asked for, and dropping one because the other is present would be the silent
// substitution the repo's precedence rules exist to prevent. The workflow's own
// `skills:` list joins the same union further down, in the engine.
//
// Names are trimmed and deduped, flag order first.
func ResolveExtraSkills(flagSkills []string) (names []string, origin string) {
	seen := map[string]bool{}
	add := func(list []string) bool {
		any := false
		for _, raw := range list {
			n := strings.TrimSpace(raw)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, n)
			any = true
		}
		return any
	}
	fromFlag := add(flagSkills)
	fromEnv := add(strings.Split(os.Getenv(EnvExtraSkills), ","))
	switch {
	case fromFlag && fromEnv:
		origin = "flag+env"
	case fromFlag:
		origin = "flag"
	case fromEnv:
		origin = "env"
	}
	return names, origin
}
