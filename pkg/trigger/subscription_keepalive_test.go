package trigger

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/schedgate"
)

func TestFromKeepaliveInvocation(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	inv := bundle.Invocation{
		Kind: bundle.InvocationKindKeepalive,
		Keepalive: &bundle.InvocationKeepalive{
			Interval:    "30s",
			StaleAfter:  "10m",
			DefaultVars: map[string]string{"mode": "watch"},
		},
	}
	sub, ok := FromKeepaliveInvocation("id1", "", "acme/repo", "bots/daemon", "operator", inv, now)
	if !ok {
		t.Fatal("expected ok for a valid keepalive invocation")
	}
	if sub.Invocation != bundle.InvocationKindKeepalive {
		t.Fatalf("Invocation = %q", sub.Invocation)
	}
	if sub.IntervalSeconds != 30 {
		t.Fatalf("IntervalSeconds = %d, want 30", sub.IntervalSeconds)
	}
	if sub.Overlap != schedgate.OverlapKeepalive || sub.StaleAfter != "10m" {
		t.Fatalf("policy fields wrong: overlap=%q stale=%q", sub.Overlap, sub.StaleAfter)
	}
	if len(sub.Match.Sources) != 1 || sub.Match.Sources[0] != SourceSchedule {
		t.Fatalf("Match.Sources = %v, want [schedule]", sub.Match.Sources)
	}
	if sub.EffectiveMode() != bundle.ExecutionDirect {
		t.Fatalf("mode = %q, want direct", sub.EffectiveMode())
	}
	if sub.Vars["mode"] != "watch" {
		t.Fatalf("default vars not propagated: %+v", sub.Vars)
	}
	// Policy() must project the keepalive overlap + stale cutoff.
	p := sub.Policy()
	if p.Overlap != schedgate.OverlapKeepalive || p.StaleAfterDuration() != 10*time.Minute {
		t.Fatalf("Policy() = %+v", p)
	}
}

func TestFromKeepaliveInvocation_WrongKind(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, ok := FromKeepaliveInvocation("id", "", "", "b", "o", bundle.Invocation{Kind: bundle.InvocationKindBoard}, now); ok {
		t.Fatal("non-keepalive invocation must yield ok=false")
	}
	// keepalive kind but interval below floor → ok=false (defensive; parse
	// validation already rejects this upstream).
	inv := bundle.Invocation{Kind: bundle.InvocationKindKeepalive, Keepalive: &bundle.InvocationKeepalive{Interval: "1s"}}
	if _, ok := FromKeepaliveInvocation("id", "", "", "b", "o", inv, now); ok {
		t.Fatal("sub-floor interval must yield ok=false")
	}
}
