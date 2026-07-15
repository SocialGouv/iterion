package cloudpublisher

import (
	"context"
	"io"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A panicking detached task must be contained AND still call
// detached.Done — otherwise Drain hangs forever on shutdown.
func TestGoSafeDetachedRecoversAndDrains(t *testing.T) {
	p := &Publisher{logger: iterlog.New(iterlog.LevelError, io.Discard)}
	ran := make(chan struct{})
	p.goSafeDetached("panicker", func() {
		defer close(ran)
		panic("boom")
	})
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("detached fn never ran")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Drain(ctx); err != nil {
		t.Fatalf("Drain after recovered panic: %v (Done not called?)", err)
	}
}
