package dispatcher

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// prRouterTracker wraps the plain fakeTracker and implements the OPTIONAL
// linkedPRProbe + labelApplier capabilities the ticket router type-asserts.
// hasPR is canned per test; applied labels are recorded.
type prRouterTracker struct {
	*fakeTracker
	hasPR bool

	mu     sync.Mutex
	labels map[string]string // issueID -> last label applied
}

func newPRRouterTracker(hasPR bool) *prRouterTracker {
	return &prRouterTracker{fakeTracker: newFakeTracker(), hasPR: hasPR, labels: map[string]string{}}
}

func (t *prRouterTracker) HasLinkedPR(_ context.Context, _ string) (bool, error) { return t.hasPR, nil }

func (t *prRouterTracker) ApplyLabel(_ context.Context, id, label string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.labels[id] = label
	return nil
}

func (t *prRouterTracker) labelFor(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.labels[id]
}

func routerTestDispatcher(t *testing.T, ft tracker.Tracker) *Dispatcher {
	t.Helper()
	dir := t.TempDir()
	wsDir := dir + "/ws"
	cfg := &Config{
		Name:         "test",
		Workflow:     t.TempDir() + "/fake.bot",
		Tracker:      TrackerConfig{Kind: "fake"},
		Polling:      PollingConfig{IntervalMS: 100},
		Agent:        AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000},
		Workspace:    WorkspaceConfig{Root: wsDir},
		Stall:        StallConfig{TimeoutMS: 0},
		TicketRouter: TicketRouterConfig{Enabled: true},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(wsDir)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    ft,
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestRouteUnassignedIssue_NoPR: an unassigned issue with no linked PR routes to
// the implement bot (Featurly) and is stamped bot:featurly.
func TestRouteUnassignedIssue_NoPR(t *testing.T) {
	ft := newPRRouterTracker(false)
	c := routerTestDispatcher(t, ft)
	iss := tracker.Issue{ID: "github:acme/widgets#5", Identifier: "widgets#5"}

	bot, ok := c.routeUnassignedIssue(context.Background(), c.cfg.Load(), iss)
	if !ok || bot != "feature-dev" {
		t.Fatalf("no-PR issue must route to feature-dev: bot=%q ok=%v", bot, ok)
	}
	if got := ft.labelFor(iss.ID); got != "bot:featurly" {
		t.Fatalf("expected bot:featurly label, got %q", got)
	}
}

// TestRouteUnassignedIssue_HasPR: an unassigned issue a PR already links makes
// the dispatcher STEP ASIDE (ok=false) — the PR-webhook owns Billy-on-PR — and
// the issue is stamped bot:billy for visibility.
func TestRouteUnassignedIssue_HasPR(t *testing.T) {
	ft := newPRRouterTracker(true)
	c := routerTestDispatcher(t, ft)
	iss := tracker.Issue{ID: "github:acme/widgets#6", Identifier: "widgets#6"}

	bot, ok := c.routeUnassignedIssue(context.Background(), c.cfg.Load(), iss)
	if ok || bot != "" {
		t.Fatalf("PR-linked issue must step aside: bot=%q ok=%v", bot, ok)
	}
	if got := ft.labelFor(iss.ID); got != "bot:billy" {
		t.Fatalf("expected bot:billy label, got %q", got)
	}
}

// TestRouteUnassignedIssue_NoProbe: a tracker WITHOUT the linked-PR capability
// degrades to the implement bot (never blocks an issue, never dedups against a
// PR-webhook it cannot observe).
func TestRouteUnassignedIssue_NoProbe(t *testing.T) {
	c := routerTestDispatcher(t, newFakeTracker()) // plain fake: no HasLinkedPR
	iss := tracker.Issue{ID: "x#1", Identifier: "x#1"}

	bot, ok := c.routeUnassignedIssue(context.Background(), c.cfg.Load(), iss)
	if !ok || bot != "feature-dev" {
		t.Fatalf("no-probe tracker must degrade to feature-dev: bot=%q ok=%v", bot, ok)
	}
}
