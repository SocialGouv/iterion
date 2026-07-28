package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func TestHTTPStateAndRefresh(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:10", Identifier: "fake#10", WorkflowState: "ready"})

	c := newTestDispatcher(t, &StubRunner{}, ft, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	srv := httptest.NewServer(c.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if snap.Tracker != "fake" {
		t.Fatalf("tracker name: %q", snap.Tracker)
	}

	r2, err := http.Post(srv.URL+"/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	if r2.StatusCode != 202 {
		t.Fatalf("status %d", r2.StatusCode)
	}
	r2.Body.Close()
}

func TestHTTPIssueDetailAndCancel(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:20", Identifier: "fake#20", WorkflowState: "ready"})

	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(c.Snapshot().Running) != 1 {
		t.Fatal("dispatch never started")
	}

	srv := httptest.NewServer(c.Routes())
	defer srv.Close()

	r1, err := http.Get(srv.URL + "/issues/fake:20")
	if err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	if r1.StatusCode != 200 {
		t.Fatalf("status %d", r1.StatusCode)
	}
	r1.Body.Close()

	r404, _ := http.Get(srv.URL + "/issues/unknown")
	if r404.StatusCode != 404 {
		t.Fatalf("want 404, got %d", r404.StatusCode)
	}
	r404.Body.Close()

	// Cancellation intentionally makes the issue eligible for dispatch again.
	// Pause new dispatches so this assertion observes the cancelled run's
	// teardown instead of racing its legitimate redispatch.
	c.Pause()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/issues/fake:20/cancel", nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	if r2.StatusCode != 202 {
		t.Fatalf("status %d", r2.StatusCode)
	}
	r2.Body.Close()

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cancel did not flush running entry")
}

func TestHTTPMethodChecks(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{}, ft, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	srv := httptest.NewServer(c.Routes())
	defer srv.Close()

	r, _ := http.Post(srv.URL+"/state", "", nil)
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("state POST: status %d", r.StatusCode)
	}
	r.Body.Close()

	r2, _ := http.Get(srv.URL + "/refresh")
	if r2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("refresh GET: status %d", r2.StatusCode)
	}
	r2.Body.Close()
}
