package binding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// fakeCatalog answers one package, or one error, for every connector.
type fakeCatalog struct {
	pkg   *spec.Package
	err   error
	asked int
}

func (c *fakeCatalog) Package(string) (*spec.Package, connection.Tier, error) {
	c.asked++
	if c.err != nil {
		return nil, connection.Tier{}, c.err
	}
	return c.pkg, connection.Tier{Kind: connection.TierDirectory}, nil
}

func (c *fakeCatalog) Connectors() ([]string, error) { return []string{"slack"}, nil }

func slackPackage() *spec.Package {
	p := &spec.Package{Triggers: []spec.Trigger{slackTrigger}}
	p.Connector.ID = "slack"
	return p
}

func connectorSub(id, tenant, connID string, filters map[string][]string) trigger.Subscription {
	return trigger.Subscription{ID: id, TenantID: tenant, BotID: "b", Enabled: true, Invocation: bundle.InvocationKindConnector,
		ConnectionID: connID, TriggerID: "slack.message.posted", Filters: filters}
}

func matcherWorld(t *testing.T, cat connection.Catalog, subs ...trigger.Subscription) *Matcher {
	t.Helper()
	st := trigger.NewMemorySubscriptionStore()
	for _, s := range subs {
		if err := st.Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	return &Matcher{Subscriptions: st, Catalog: func(string) connection.Catalog { return cat }}
}

func slackIntake(tenant string) intake.Intake {
	return intake.Intake{ID: "in-1", TenantID: tenant, ConnectionID: "conn-a", IngressID: "ing-1",
		TriggerID: "slack.message.posted", Event: []byte(storedEvent)}
}

// One verdict per candidate of the intake's own connection, in
// subscription-id order, each the subscription's own judgment.
func TestTheMatcherJudgesItsConnectionsCandidates(t *testing.T) {
	m := matcherWorld(t, &fakeCatalog{pkg: slackPackage()},
		connectorSub("s2", "t1", "conn-a", map[string][]string{"channel": {"C1"}}),
		connectorSub("s1", "t1", "conn-a", map[string][]string{"channel": {"C2"}}),
		connectorSub("s3", "t1", "conn-b", nil),
	)
	vs, err := m.Match(context.Background(), slackIntake("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].SubscriptionID != "s1" || vs[1].SubscriptionID != "s2" {
		t.Fatalf("verdicts %+v, want s1 then s2", vs)
	}
	if vs[0].Matched || vs[0].Reason == "" || !vs[1].Matched {
		t.Fatalf("verdicts %+v, want s1 not matched with its reason, s2 matched", vs)
	}
}

// The local scope stores its intakes under connection.LocalTenant and its
// subscriptions under "".
func TestTheMatcherReadsTheLocalSubscriptions(t *testing.T) {
	m := matcherWorld(t, &fakeCatalog{pkg: slackPackage()}, connectorSub("s1", "", "conn-a", nil))
	vs, err := m.Match(context.Background(), slackIntake(connection.LocalTenant))
	if err != nil || len(vs) != 1 || !vs[0].Matched {
		t.Fatalf("a local intake = %+v, %v; want the local subscription matched", vs, err)
	}
}

// What no retry changes refuses every candidate, naming why; an outage is
// an error the worker retries.
func TestTheMatcherRefusesWhatNoRetryChanges(t *testing.T) {
	sub := connectorSub("s1", "t1", "conn-a", nil)
	undeclared := slackPackage()
	undeclared.Triggers = nil
	for name, tc := range map[string]struct {
		cat  connection.Catalog
		in   func(intake.Intake) intake.Intake
		want string
	}{
		"connector gone":     {connection.NewFSCatalog(t.TempDir()), nil, "no longer holds connector"},
		"package unloadable": {&fakeCatalog{err: errors.New("bad yaml")}, nil, "does not load"},
		"trigger undeclared": {&fakeCatalog{pkg: undeclared}, nil, "no longer declares trigger"},
		"event undecodable":  {&fakeCatalog{pkg: slackPackage()}, func(in intake.Intake) intake.Intake { in.Event = []byte(`{"payload":`); return in }, "decode"},
	} {
		m := matcherWorld(t, tc.cat, sub)
		in := slackIntake("t1")
		if tc.in != nil {
			in = tc.in(in)
		}
		vs, err := m.Match(context.Background(), in)
		if err != nil {
			t.Errorf("%s: answered an error %v, want a refused pair", name, err)
			continue
		}
		if len(vs) != 1 || !strings.Contains(vs[0].Refusal, tc.want) {
			t.Errorf("%s: verdicts %+v, want a refusal containing %q", name, vs, tc.want)
		}
	}

	down := &fakeCatalog{err: &connection.CatalogUnavailableError{Tier: connection.TierDirectory, Connector: "slack", Err: errors.New("disk")}}
	if _, err := matcherWorld(t, down, sub).Match(context.Background(), slackIntake("t1")); !connection.IsCatalogUnavailable(err) {
		t.Errorf("a catalog outage = %v, want the outage itself, retried", err)
	}

	idle := &fakeCatalog{err: errors.New("not asked")}
	if vs, err := matcherWorld(t, idle).Match(context.Background(), slackIntake("t1")); err != nil || len(vs) != 0 || idle.asked != 0 {
		t.Errorf("an intake with no candidate = %v, %v (catalog asked %d times); want no verdict and no catalog read", vs, err, idle.asked)
	}
}

// Only the context's end interrupts a match: the intake is retried, no
// verdict is half-written.
func TestAMatchStopsWithItsContext(t *testing.T) {
	sub := connectorSub("s1", "t1", "conn-a", nil)
	sub.When = "event.count > 1"
	m := matcherWorld(t, &fakeCatalog{pkg: slackPackage()}, sub)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if vs, err := m.Match(ctx, slackIntake("t1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled match = %+v, %v; want the context's end", vs, err)
	}
}

// A tenant with no catalog is an outage the worker retries, not a refusal.
func TestAMissingCatalogIsRetried(t *testing.T) {
	m := matcherWorld(t, nil, connectorSub("s1", "t1", "conn-a", nil))
	m.Catalog = func(string) connection.Catalog { return nil }
	if vs, err := m.Match(context.Background(), slackIntake("t1")); err == nil {
		t.Fatalf("a missing catalog = %+v, want an error", vs)
	}
}

// The local scope's two tenants map onto each other, and a cloud tenant is
// itself on both sides.
func TestTheLocalTenantsMapBothWays(t *testing.T) {
	if got := ConnectionTenant(""); got != connection.LocalTenant {
		t.Errorf("ConnectionTenant(\"\") = %q, want %q", got, connection.LocalTenant)
	}
	if got := SubscriptionTenant(connection.LocalTenant); got != "" {
		t.Errorf("SubscriptionTenant(local) = %q, want the local scope", got)
	}
	for _, team := range []string{"t1", "team-42"} {
		if ConnectionTenant(team) != team || SubscriptionTenant(team) != team {
			t.Errorf("team %q did not map to itself", team)
		}
	}
}
