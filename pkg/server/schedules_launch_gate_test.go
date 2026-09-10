package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
)

// A cloud schedule is a launch like any other (#844): before this, a team
// could schedule its way around every cap — the tick launched with neither
// admission nor metering, so a suspended org, an org at its concurrency cap
// and an org past its monthly quota all kept firing every cron slot for free.

func scheduledBot(tenant string) cloudsched.ScheduledBot {
	return cloudsched.ScheduledBot{
		ID: "sb-1", TenantID: tenant, BotID: "probe", Cron: "* * * * *",
		NextFireAt: time.Now().UTC(),
	}
}

func TestLaunchScheduledBot_GateDenialRefusesTheTick(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", maxConcurrentRuns: 1}, pub)
	s.cfg.Store = fakeActiveStore{RunStore: s.cfg.Store, active: 1}

	err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1"))
	var deny *launchDeniedError
	if !errors.As(err, &deny) || deny.Reason != denyConcurrencyCap {
		t.Fatalf("launchScheduledBot = %v, want the gate's typed %s denial", err, denyConcurrencyCap)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("the run service was asked %d time(s) at the org's concurrency cap, want 0", got)
	}
}

func TestLaunchScheduledBot_SuspendedTeamCannotFire(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", teamStatus: identity.TeamStatusSuspended}, pub)

	err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1"))
	var deny *launchDeniedError
	if !errors.As(err, &deny) || deny.Reason != denyOrgSuspended {
		t.Fatalf("launchScheduledBot = %v, want the gate's typed %s denial", err, denyOrgSuspended)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("a suspended team fired %d scheduled run(s), want 0", got)
	}
}

func TestLaunchScheduledBot_MetersTheTickAndRollsBackARefusal(t *testing.T) {
	t.Run("fired tick meters one slot", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 3}, pub)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter

		if err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1")); err != nil {
			t.Fatalf("launchScheduledBot = %v, want nil", err)
		}
		u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
		if u.Runs != 1 {
			t.Errorf("monthly runs = %d after one scheduled launch, want 1", u.Runs)
		}
	})
	t.Run("refused launch releases the slot", func(t *testing.T) {
		pub := &spinePublisher{err: errors.New("cloudpublisher: queue unavailable")}
		s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 3}, pub)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter

		if err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1")); err == nil {
			t.Fatal("launchScheduledBot = nil, want the run service's refusal")
		}
		u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
		if u.Runs != 0 {
			t.Errorf("monthly runs = %d after a refused launch, want 0", u.Runs)
		}
	})
	t.Run("the quota is enforced, not just counted", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 1}, pub)
		s.orgUsage = orgusage.NewMemoryCounter()

		if err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1")); err != nil {
			t.Fatalf("first tick = %v, want nil", err)
		}
		err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1"))
		var deny *launchDeniedError
		if !errors.As(err, &deny) || deny.Reason != denyMonthlyRunQuota {
			t.Fatalf("second tick = %v, want the gate's typed %s denial", err, denyMonthlyRunQuota)
		}
		if got := pub.count(); got != 1 {
			t.Errorf("the run service was asked %d time(s) for a quota of 1, want 1", got)
		}
	})
}
