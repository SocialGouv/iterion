package storekit

import (
	"testing"
	"time"
)

type payload struct{ V string }

func TestNewTicket(t *testing.T) {
	a, err := NewTicket()
	if err != nil {
		t.Fatalf("NewTicket: %v", err)
	}
	b, err := NewTicket()
	if err != nil {
		t.Fatalf("NewTicket: %v", err)
	}
	if a == b {
		t.Fatal("two tickets identical")
	}
	if len(a) != 43 { // 32 bytes base64url, unpadded
		t.Fatalf("ticket length = %d, want 43", len(a))
	}
}

func TestTicketMemory_SingleUse(t *testing.T) {
	s := NewTicketMemory[payload]()
	ticket, err := s.Mint(payload{V: "x"}, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, ok := s.Redeem(ticket)
	if !ok || got.V != "x" {
		t.Fatalf("Redeem = %+v, %v", got, ok)
	}
	if _, ok := s.Redeem(ticket); ok {
		t.Fatal("second Redeem must fail (single use)")
	}
	if _, ok := s.Redeem("nope"); ok {
		t.Fatal("unknown Redeem must fail")
	}
}

func TestTicketMemory_ExpiryAndSweep(t *testing.T) {
	s := NewTicketMemory[payload]()
	expired, err := s.Mint(payload{V: "old"}, -time.Second)
	if err != nil {
		t.Fatalf("Mint expired: %v", err)
	}
	if _, ok := s.Redeem(expired); ok {
		t.Fatal("expired Redeem must fail")
	}
	// A later Mint sweeps expired entries.
	stale, _ := s.Mint(payload{V: "stale"}, -time.Second)
	if _, err := s.Mint(payload{V: "fresh"}, time.Minute); err != nil {
		t.Fatalf("Mint fresh: %v", err)
	}
	if len(s.m) != 1 {
		t.Fatalf("post-sweep entries = %d, want 1 (stale %q swept)", len(s.m), stale)
	}
}

func TestTicketMemory_DistinctTickets(t *testing.T) {
	s := NewTicketMemory[payload]()
	a, _ := s.Mint(payload{V: "a"}, time.Minute)
	b, _ := s.Mint(payload{V: "b"}, time.Minute)
	if a == b {
		t.Fatal("two mints produced the same ticket")
	}
	ra, _ := s.Redeem(a)
	rb, _ := s.Redeem(b)
	if ra.V != "a" || rb.V != "b" {
		t.Fatalf("tickets crossed: a=%q b=%q", ra.V, rb.V)
	}
}
