package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
)

func TestWSTicket_MintRedeemCycle(t *testing.T) {
	s := &Server{wsTickets: wsticket.NewMemoryStore(time.Minute)}
	id := auth.Identity{UserID: "u1", Email: "a@b.io", TeamID: "team-1"}

	// Mint (handler runs behind requireAuth, so the identity is in context).
	r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	r = r.WithContext(auth.WithIdentity(r.Context(), id))
	w := httptest.NewRecorder()
	s.handleWSTicket(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("mint status = %d, want 200", w.Code)
	}
	var body struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Ticket == "" || body.ExpiresIn <= 0 {
		t.Fatalf("bad mint response: %+v", body)
	}

	// Redeem via the WS path resolver.
	wsReq := httptest.NewRequest(http.MethodGet, "/api/ws/runs/abc?ticket="+body.Ticket, nil)
	got, ok := s.wsTicketIdentity(wsReq)
	if !ok || got.UserID != "u1" || got.TeamID != "team-1" {
		t.Fatalf("wsTicketIdentity = %+v, ok=%v; want u1/team-1", got, ok)
	}

	// Single use: a second redeem is "handled but rejected" (ok=true, empty id).
	got2, ok2 := s.wsTicketIdentity(wsReq)
	if !ok2 || got2.UserID != "" {
		t.Errorf("second redeem = %+v ok=%v; want ok=true empty identity", got2, ok2)
	}

	// The Browser-Live CDP path also accepts ?ticket= (workspace pane dials it
	// cross-origin without a JWT in the URL).
	ticket2, err := s.wsTickets.Mint(r.Context(), id)
	if err != nil {
		t.Fatalf("mint 2: %v", err)
	}
	cdpReq := httptest.NewRequest(http.MethodGet, "/api/runs/abc/browser/cdp?session=s1&ticket="+ticket2, nil)
	got3, ok3 := s.wsTicketIdentity(cdpReq)
	if !ok3 || got3.UserID != "u1" {
		t.Fatalf("CDP ticket auth = %+v ok=%v; want u1", got3, ok3)
	}
}

func TestWSTicketIdentity_NonTicketFallsThrough(t *testing.T) {
	s := &Server{wsTickets: wsticket.NewMemoryStore(time.Minute)}

	// Not a WS path → false (fall through to normal auth).
	r := httptest.NewRequest(http.MethodGet, "/api/runs?ticket=x", nil)
	if _, ok := s.wsTicketIdentity(r); ok {
		t.Error("non-WS path should fall through (ok=false)")
	}
	// WS path but no ?ticket= → false (use bearer/?t=).
	r = httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	if _, ok := s.wsTicketIdentity(r); ok {
		t.Error("WS path without ticket should fall through (ok=false)")
	}
	// WS path with an unknown ticket → handled+rejected (ok=true, empty id).
	r = httptest.NewRequest(http.MethodGet, "/api/ws?ticket=bogus", nil)
	id, ok := s.wsTicketIdentity(r)
	if !ok || id.UserID != "" {
		t.Errorf("bogus ticket = %+v ok=%v; want ok=true empty id", id, ok)
	}
}
