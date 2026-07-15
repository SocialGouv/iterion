package server

import (
	"io"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestGoSafeRecoversPanic(t *testing.T) {
	s := &Server{logger: iterlog.New(iterlog.LevelError, io.Discard)}
	done := make(chan struct{})
	// Without the recover inside goSafe, this panic crashes the whole
	// test binary (an unrecovered goroutine panic aborts the process).
	s.goSafe("panicker", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goSafe fn never ran")
	}
	// Process still alive → goSafe also works for a normal fn.
	var wg sync.WaitGroup
	wg.Add(1)
	s.goSafe("normal", func() { wg.Done() })
	wg.Wait()
}

func TestGoSafeNilLoggerDoesNotCrash(t *testing.T) {
	s := &Server{} // logger nil — recover() still runs (first half of &&)
	done := make(chan struct{})
	s.goSafe("panicker", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goSafe fn never ran")
	}
}
