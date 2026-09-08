package runview

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// #871 — the run says which bot-source tier served it, on the wire. The
// tier being persisted but unreadable is what let an inert tier read as a
// working one for four launch surfaces; an operator has to be able to see
// the answer without opening the database.
//
// The three cases are asserted on the MARSHALLED header, not the struct,
// because the contract that matters to a reader is the JSON one: an
// unrecorded tier must be ABSENT, never rendered as a claim.
func TestHeaderFromRun_ExposesTheBotSourceTier(t *testing.T) {
	cases := []struct {
		name       string
		run        store.Run
		wantTier   string // "" = the key must be absent
		wantTenant string
	}{
		{
			name:       "a team fork names the team that owns the row",
			run:        store.Run{ID: "r-team", BotSourceTier: store.BotSourceTierTeam, BotSourceTenant: "t1"},
			wantTier:   "team",
			wantTenant: "t1",
		},
		{
			name:     "the baked catalog says so positively",
			run:      store.Run{ID: "r-baked", BotSourceTier: store.BotSourceTierBaked},
			wantTier: "baked",
		},
		{
			name:     "a run that recorded no tier claims none",
			run:      store.Run{ID: "r-local"},
			wantTier: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := headerFromRun(&tc.run)
			if h.BotSourceTier != tc.wantTier {
				t.Errorf("header.BotSourceTier = %q, want %q", h.BotSourceTier, tc.wantTier)
			}
			if h.BotSourceTenant != tc.wantTenant {
				t.Errorf("header.BotSourceTenant = %q, want %q", h.BotSourceTenant, tc.wantTenant)
			}
			b, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatal(err)
			}
			got, present := wire["bot_source_tier"]
			switch {
			case tc.wantTier == "" && present:
				t.Errorf("bot_source_tier = %v on a run that recorded none — absence must stay absence, or an unresolved tier reads as a deliberate one", got)
			case tc.wantTier != "" && got != tc.wantTier:
				t.Errorf("wire bot_source_tier = %v, want %q", got, tc.wantTier)
			}
			if _, present := wire["bot_source_tenant"]; present != (tc.wantTenant != "") {
				t.Errorf("bot_source_tenant present=%v, want %v", present, tc.wantTenant != "")
			}
		})
	}
}
