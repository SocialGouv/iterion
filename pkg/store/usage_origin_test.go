package store_test

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"testing"
)

func TestUsageOriginFS(t *testing.T) {
	storetest.RunUsageOriginConformance(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
func TestUsageOriginFreeze(t *testing.T) {
	origin := store.UsageOrigin{Version: 1, RunID: "r", Publication: "2026-11-04T02:00:00+02:00", TenantID: "t", OrgID: "original"}
	frozen, e := store.FreezeInitialUsageReservation(store.OutputCorrectionReservation{Origin: &origin})
	if e != nil {
		t.Fatal(e)
	}
	origin.OrgID = "changed"
	if frozen.Origin.OrgID != "original" || frozen.Origin.Publication != "2026-11-04T00:00:00Z" {
		t.Fatal("origin not canonical/copied")
	}
}
