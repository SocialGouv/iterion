package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

type observedScheduleStore struct {
	*cloudsched.MemoryStore
	listDue chan struct{}
}

func (s *observedScheduleStore) ListDue(ctx context.Context, now time.Time, limit int) ([]cloudsched.ScheduledBot, error) {
	select {
	case s.listDue <- struct{}{}:
	default:
	}
	return s.MemoryStore.ListDue(ctx, now, limit)
}

func TestSupersededServerServesDiagnosticsWithoutStartingWorkers(t *testing.T) {
	store := &observedScheduleStore{
		MemoryStore: cloudsched.NewMemoryStore(),
		listDue:     make(chan struct{}, 1),
	}
	srv := New(Config{
		Bind:          "127.0.0.1",
		Port:          0,
		Superseded:    true,
		ScheduledBots: store,
	}, iterlog.New(iterlog.LevelError, nil))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	select {
	case <-store.listDue:
		t.Fatal("superseded server started the cloud scheduler")
	case <-time.After(100 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ListenAndServe: %v", err)
	}
}
