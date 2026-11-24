package runner

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The wall an exhausted chain parks on is read from the route that reopens
// FIRST — the resume re-walks the chain and proceeds on it — whatever order
// the walk met its routes in. errors.As finds the first typed refusal of the
// walk, and the chain's message names its last route: neither is a fact
// about which route reopens first, so a chain parked on a weekly wall when a
// five-hour route reopened that afternoon, or lost a dated wall behind a
// throttle and spent its attempts on blind hourly guesses (#3127).

// chainRoute is one route of the chain under test.
type chainRoute struct {
	name string
	err  func(now time.Time) error
}

func datedRoute(name, words string, at time.Time) chainRoute {
	return chainRoute{name: name, err: func(now time.Time) error {
		reset, ok := delegate.ParseResetHint(fmt.Sprintf(words, at.In(facadeClock).Format("2006-01-02 15:04:05")), now)
		if !ok {
			panic("fixture: the facade shape no longer parses")
		}
		return &delegate.ErrRateLimited{Provider: "zai", Kind: delegate.RateLimitKindUsageWindow,
			Detail: fmt.Sprintf(words, at.In(facadeClock).Format("2006-01-02 15:04:05")), ResetAt: reset}
	}}
}

var (
	routeThrottle = chainRoute{name: "throttle", err: func(time.Time) error {
		return &delegate.ErrRateLimited{Provider: "zai", Kind: delegate.RateLimitKindTransient, Detail: "429 too many requests"}
	}}
	routeDeadCredential = chainRoute{name: "auth", err: func(time.Time) error {
		return &delegate.ErrAuthFailed{Provider: delegate.BackendClaudeCode, Detail: "OAuth session expired and could not be refreshed"}
	}}
)

// chainPermutations returns every ordering of routes.
func chainPermutations(routes []chainRoute) [][]chainRoute {
	if len(routes) <= 1 {
		return [][]chainRoute{append([]chainRoute(nil), routes...)}
	}
	var out [][]chainRoute
	for i := range routes {
		rest := append(append([]chainRoute(nil), routes[:i]...), routes[i+1:]...)
		for _, p := range chainPermutations(rest) {
			out = append(out, append([]chainRoute{routes[i]}, p...))
		}
	}
	return out
}

// nodeFailure is the error the runner parks on: the engine's RuntimeError,
// coded by the real classifier, around the executor's exhausted chain.
func nodeFailure(routes []chainRoute, now time.Time) error {
	errs := make([]error, len(routes))
	for i, r := range routes {
		errs[i] = r.err(now)
	}
	chain := fmt.Errorf("model: node %q: %w", "review", &model.ErrChainExhausted{Chain: "zai,run-fallback,zai", Errs: errs})
	return &runtime.RuntimeError{Code: runtime.ClassifyError(chain), Message: "node review failed", NodeID: "review", Cause: chain}
}

func TestUsageWindow_AChainParksOnTheRouteThatReopensFirstInEveryOrder(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	weekly := datedRoute("weekly", "[1310][Weekly/Monthly Limit Exhausted. Your limit will reset at %s]", now.Add(96*time.Hour))
	fiveHour := datedRoute("5h", "[1308][Usage limit reached for 5 hour. Your limit will reset at %s]", now.Add(3*time.Hour))
	pol := noJitter(retrypolicy.Policy{MaxWait: "192h"})
	wantWall := now.Add(3 * time.Hour)

	for _, multiset := range [][]chainRoute{
		{weekly, fiveHour},
		{fiveHour, routeThrottle, routeDeadCredential},
		{weekly, fiveHour, routeDeadCredential},
	} {
		for _, order := range chainPermutations(multiset) {
			names := make([]string, len(order))
			for i, r := range order {
				names[i] = r.name
			}
			label := strings.Join(names, ",")
			execErr := nodeFailure(order, now)
			at, source, ok := usageWindowRetryAt(execErr, pol, now, time.Time{}, 0)
			if !ok {
				t.Fatalf("[%s]: no usage retry armed", label)
			}
			if want := wantWall.Add(retrypolicy.UsageWindowComeBack); !at.Equal(want) {
				t.Errorf("[%s]: wake = %s (source %s), want %s — the five-hour route reopens first", label, at.Format(time.RFC3339), source, want.Format(time.RFC3339))
			}
			wall, dated, ok := usageRefusalWall(execErr, now)
			if !ok || !wall.Equal(wantWall) || !dated {
				t.Errorf("[%s]: wall = %s dated=%v ok=%v, want the dated %s", label, wall.Format(time.RFC3339), dated, ok, wantWall.Format(time.RFC3339))
			}
		}
	}
}

// Whether a chain's wall is DATED is asked of the deciding route's own words:
// a five-hour window the facade renders from its duration is no news about
// the wall (usageParkWall keeps the row's), in every order — the chain's
// message names its last route, whose words say nothing about this one.
func TestUsageWindow_AChainsWallIsDatedByTheDecidingRoutesOwnWords(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rendered := datedRoute("5h-rendered", "[1308][Usage limit reached for 5 hour. Your limit will reset at %s]", now.Add(5*time.Hour))
	for _, order := range chainPermutations([]chainRoute{rendered, routeDeadCredential}) {
		label := order[0].name + "," + order[1].name
		wall, dated, ok := usageRefusalWall(nodeFailure(order, now), now)
		if !ok || !wall.Equal(now.Add(5*time.Hour)) {
			t.Fatalf("[%s]: wall = %s ok=%v, want the rendered %s", label, wall.Format(time.RFC3339), ok, now.Add(5*time.Hour).Format(time.RFC3339))
		}
		if dated {
			t.Errorf("[%s]: dated = true — a duration rendering is no news about the wall, whichever route the chain names last", label)
		}
	}
}
