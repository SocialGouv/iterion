package runview

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"testing"
)

func TestBrokerReviewCancelDoesNotReleaseOwner(t *testing.T) {
	o, err := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1, BirthID: "a"}, "t", true)
	if err != nil {
		t.Fatal(err)
	}
	b := NewEventBroker()
	h, err := b.AcquireObserved(o)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	first, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	first.Cancel()
	first.Cancel()
	select {
	case _, ok := <-first.C:
		if ok {
			t.Fatal("canceled subscription open")
		}
	default:
		t.Fatal("Cancel did not close")
	}
	next, err := h.Subscribe()
	if err != nil {
		t.Fatal("Cancel released owner", err)
	}
	defer next.Cancel()
	evt, err := store.WithEventObservation(store.Event{RunID: "r"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Publish(evt); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-next.C:
		if got == nil {
			t.Fatal("owner retired")
		}
	default:
		t.Fatal("owner publication lost")
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-next.C:
		if ok {
			t.Fatal("unexpected queued row")
		}
	default:
		t.Fatal("last owner did not close")
	}
}

func TestBrokerReviewCopiedCloseConcurrent(t *testing.T) {
	o, err := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1, BirthID: "a"}, "t", true)
	if err != nil {
		t.Fatal(err)
	}
	b := NewEventBroker()
	h1, err := b.AcquireObserved(o)
	if err != nil {
		t.Fatal(err)
	}
	defer h1.Close()
	h2, err := b.AcquireObserved(o)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	sub, err := h1.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	copies := make([]ObservedEventScope, 32)
	for i := range copies {
		copies[i] = *h1
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range copies {
		wg.Add(1)
		go func(h *ObservedEventScope) {
			defer wg.Done()
			<-start
			if err := h.Close(); err != nil {
				t.Error(err)
			}
		}(&copies[i])
	}
	close(start)
	wg.Wait()
	evt, err := store.WithEventObservation(store.Event{RunID: "r"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = h2.Publish(evt); err != nil {
		t.Fatal("copies retired H2", err)
	}
	select {
	case v, ok := <-sub.C:
		if !ok || v == nil {
			t.Fatal("copies closed shared subscriber")
		}
	default:
		t.Fatal("H2 publication absent")
	}
	for i := range copies {
		if err = copies[i].Publish(evt); !errors.Is(err, store.ErrRunDataCapability) {
			t.Fatal("copy not closed", err)
		}
	}
	h2.Close()
	replacement, err := b.AcquireObserved(o)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	fresh, err := replacement.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Cancel()
	sub.Cancel()
	h1.Close()
	for i := range copies {
		copies[i].Close()
	}
	if err = replacement.Publish(evt); err != nil {
		t.Fatal(err)
	}
	select {
	case v, ok := <-fresh.C:
		if !ok || v == nil {
			t.Fatal("old release closed replacement")
		}
	default:
		t.Fatal("replacement publication absent")
	}
}
