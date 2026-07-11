package secrets

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// UnresolvedRequired returns, sorted, the names in `required` that are absent
// from `resolved` (the set of workflow-secret names that resolved to a
// non-empty value for this run).
//
// It is the launch-time gate behind the required-secret contract: a workflow
// declares a secret non-`optional` precisely because the run cannot do correct
// work without it (push with no auth, call an API unauthenticated). If such a
// secret resolves to nothing — no store match, no binding, no override — the
// launch must fail loudly here rather than the runner silently skipping the
// empty value (`optional/unresolved → skip`) and the bot proceeding blind.
// `optional: true` secrets are never passed in `required`, so they keep the
// skip behaviour.
func UnresolvedRequired(required []string, resolved map[string]bool) []string {
	var missing []string
	seen := make(map[string]bool, len(required))
	for _, name := range required {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if !resolved[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// RequiredSecretsError formats the launch-blocking error naming every
// unresolved required secret. `scope` describes the resolution context so the
// operator knows where the lookup came up empty (e.g. "this team/bot" for a
// cloud launch, "this workspace" for a local run). Returns nil when nothing is
// missing so callers can `if err := RequiredSecretsError(...); err != nil`.
func RequiredSecretsError(missing []string, scope string) error {
	if len(missing) == 0 {
		return nil
	}
	if strings.TrimSpace(scope) == "" {
		scope = "this run"
	}
	parts := make([]string, len(missing))
	for i, name := range missing {
		parts[i] = fmt.Sprintf("secret %q is declared required by the workflow but resolves to nothing for %s", name, scope)
	}
	return errors.New(strings.Join(parts, "; "))
}
