// The tenant's provider allowlist at the walk (ADR-129 §5): a non-empty
// RouteView.AllowedProviders turns the walk from an attribution question
// ("what may this run spend?") into an enforcement question ("is every
// route inside what the tenant wrote?"), and the publisher refuses a
// launch whose routes cannot be answered inside it.

package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// UnreadableRoute is a route the walk could not read while an allowlist
// was in force (ProviderResolution.Unreadable): the site the route lives
// at (the fallbackSite keys — the node id, `node#route`,
// `run-fallback#i`, `supervisor:<name>`) and the field that stayed
// undecided. Hint carries the spelling that stayed as written when the
// field's own text named something.
type UnreadableRoute struct {
	Site  string
	Field string
	Hint  string
}

// ErrRouteUnreadableUnderAllowlist is the typed refusal for a route the
// walk cannot read under a non-empty provider allowlist (ADR-129 §5: a
// lock that cannot be checked must not pass). Match with errors.Is; the
// carrying struct names the site and the field.
var ErrRouteUnreadableUnderAllowlist = errors.New("a route this launch cannot be checked against its provider allowlist")

// RouteUnreadableError carries the site and field of a route the walk
// could not read under the allowlist, so the refusal names what to fix:
// resolve the field, or launch without the providers list.
type RouteUnreadableError struct {
	Site  string
	Field string
	Hint  string
}

// fieldlessSite marks the fieldless site class (a model-calling node with
// no LLMFields — a subbot): the route spends a model no field exposes. A
// human node's LLM half is not of this class (its model is read as the
// direct generation it is — humanLLMModelOf), and neither is a Verified
// Action's recovery ladder, read rung by rung as dispatch spends it
// (recoveryRungsOf, #2783).
const fieldlessSite = "\x00fieldless"

func (e *RouteUnreadableError) Error() string {
	if e.Field == fieldlessSite {
		return fmt.Sprintf("provider allowlist: node %s spends a model no field exposes (a subbot) — a non-empty providers list refuses a launch whose routes it cannot check, whatever the list names; replace or remove the node, or launch without the providers list", e.Site)
	}
	hint := ""
	if e.Hint != "" {
		hint = fmt.Sprintf(" (names %q)", e.Hint)
	}
	return fmt.Sprintf("provider allowlist: route %q: %s is unreadable%s — a non-empty providers list refuses a launch whose routes it cannot check; resolve the field (spell the provider prefix, declare the var) or launch without the providers list", e.Site, e.Field, hint)
}

// Is matches the sentinel, so errors.Is works against the class rather
// than the pointer.
func (e *RouteUnreadableError) Is(target error) bool {
	return target == ErrRouteUnreadableUnderAllowlist
}

// ErrProviderOutsideAllowlist is the typed refusal for a PRIMARY route
// that resolves to a provider outside the allowlist with no allowed route
// left to serve the node (ADR-129 §5: refused, never re-routed).
var ErrProviderOutsideAllowlist = errors.New("a primary route resolves outside the tenant's provider allowlist")

// ProviderOutsideError carries the site and provider of the unservable
// primary route and the level that set the list, so the refusal names
// what to fix.
type ProviderOutsideError struct {
	Site     string
	Provider string
	Level    string
}

func (e *ProviderOutsideError) Error() string {
	level := e.Level
	if level == "" {
		level = "unknown"
	}
	return fmt.Sprintf("provider allowlist (%s level): node %s pins provider %q, which the tenant does not allow — refused, never re-routed; widen providers or repin the node", level, e.Site, e.Provider)
}

// Is matches the sentinel, so errors.Is works against the class rather
// than the pointer.
func (e *ProviderOutsideError) Is(target error) bool {
	return target == ErrProviderOutsideAllowlist
}

// AllowedProviderSet is the allowlist in the form the walk and the
// dispatch filter compare against: lower-cased, trimmed, non-empty names.
// Nil for an empty list — no allowlist is in force.
func AllowedProviderSet(providers []string) map[string]bool {
	if len(providers) == 0 {
		return nil
	}
	out := make(map[string]bool, len(providers))
	for _, p := range providers {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out[p] = true
		}
	}
	return out
}

// sortedDedupSites is the Pins rendering for the Forbidden maps: each
// site list sorted and deduplicated, the map itself keyed in sorted order
// by its provider names for a deterministic answer.
func sortedDedupSites(m map[string][]string) map[string][]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]string, len(m))
	for p, sites := range m {
		sorted := append([]string(nil), sites...)
		sort.Strings(sorted)
		deduped := sorted[:0]
		for i, s := range sorted {
			if i == 0 || s != sorted[i-1] {
				deduped = append(deduped, s)
			}
		}
		out[p] = deduped
	}
	return out
}

// sortDedupUnreadable renders the walk's unreadable routes: sorted by
// site, field, hint; exact duplicates (a route two obligations both flag)
// held once.
func sortDedupUnreadable(in []UnreadableRoute) []UnreadableRoute {
	if len(in) == 0 {
		return nil
	}
	out := append([]UnreadableRoute(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Hint < out[j].Hint
	})
	deduped := out[:0]
	for i := range out {
		if i == 0 || out[i] != out[i-1] {
			deduped = append(deduped, out[i])
		}
	}
	return deduped
}

// FirstUnreadable is the deterministic head of the unreadable list (the
// refusal names the first by site): nil when the walk read everything.
func FirstUnreadable(res ProviderResolution) *UnreadableRoute {
	if len(res.Unreadable) == 0 {
		return nil
	}
	u := res.Unreadable[0]
	return &u
}

// FirstForbiddenPrimary is the deterministic head of the unservable
// primaries — the smallest provider name, its first site — so a refusal
// never depends on map order. ok is false when every primary route the
// walk read is servable inside the list.
func FirstForbiddenPrimary(res ProviderResolution) (site, provider string, ok bool) {
	if len(res.ForbiddenPrimary) == 0 {
		return "", "", false
	}
	provs := make([]string, 0, len(res.ForbiddenPrimary))
	for p := range res.ForbiddenPrimary {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	return res.ForbiddenPrimary[provs[0]][0], provs[0], true
}

// ProviderAllowlistNames renders the allowlist for a refusal: sorted
// names, "none set" when no list is in force (a refusal that cannot
// happen then — the filter is nil).
func ProviderAllowlistNames(allowed map[string]bool) string {
	if len(allowed) == 0 {
		return "none set"
	}
	names := make([]string, 0, len(allowed))
	for p := range allowed {
		names = append(names, p)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
