package usagecap

import (
	"slices"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Equal timestamps are common across replicas (and Mongo stores milliseconds).
// An otherwise identical refusal with a later reset must not randomly lose to
// an already rolled-over window when map/cursor iteration order changes.
func TestAccountReadingEqualTimestampIsOrderIndependent(t *testing.T) {
	now := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	base := Reading{Window: WindowSevenDay, Status: StatusRejected, ObservedAt: now.Add(-time.Minute)}
	expired, future := base, base
	expired.ResetsAt, future.ResetsAt = now.Add(-time.Second), now.Add(time.Hour)
	allowed := future
	allowed.Status = StatusAllowed
	warning := allowed
	warning.Status = StatusWarning
	for _, pair := range [][2]Reading{{expired, future}, {allowed, warning}} {
		var first []Reading
		for _, order := range [][]Reading{{pair[0], pair[1]}, {pair[1], pair[0]}} {
			merged := map[Window]Reading{}
			for _, reading := range order {
				mergeAccountReading(merged, reading)
			}
			got := []Reading{merged[WindowSevenDay]}
			if first == nil {
				first = got
			} else if !slices.Equal(first, got) {
				t.Fatalf("unchanged account readings depend on iteration order: %+v vs %+v", first, got)
			}
		}
	}
	merged := map[Window]Reading{}
	mergeAccountReading(merged, future)
	mergeAccountReading(merged, expired)
	got := merged[WindowSevenDay]
	policy := Policy{Week: WindowPolicy{MaxPercent: 85, Mode: ModeHard}}
	if !got.ResetsAt.Equal(future.ResetsAt) || !Preflight([]Reading{got}, policy, now, DefaultTrust()).Blocked {
		t.Fatalf("tied refusal lost its future reset: %+v", got)
	}
}

func runAccountConformance(t *testing.T, st Store) {
	t.Helper()
	fp := (secrets.OAuthAccount{ID: "c7f30bce-7e8b-4bb7-8199-afc172f3d580", OrganizationID: "0d05ce64-6b67-4368-909c-0d26f5a5870a"}).Fingerprint()
	global := Key("claude_code", ScopePlatform, fp)
	if global != Key("claude_code", TenantScope("other-team"), fp) || global != Key("claude_code", OrgScope("org-a"), fp) {
		t.Fatal("verified account opens separate meters per tier")
	}
	if Key("claude_code", TenantScope("a"), "unverified") == Key("claude_code", TenantScope("b"), "unverified") {
		t.Fatal("legacy hash unexpectedly became a shared account")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	write := func(key string, util float64, at time.Time) {
		t.Helper()
		if err := st.Record(t.Context(), key, Reading{Window: WindowSevenDay, Utilization: util, Status: StatusAllowed, ObservedAt: at, ResetsAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	write(global, .2, now)
	// A rolling-upgrade pod running the old Key function still scopes the
	// new publisher's fingerprint. New readers must see its newer refusal.
	oldScoped := "claude_code|tenant:rolling-pod|fp:" + fp
	write(oldScoped, .99, now.Add(time.Minute))
	write("other-backend|tenant:rolling-pod|fp:"+fp, .01, now.Add(2*time.Minute))
	write(global, .1, now.Add(-time.Minute))
	for _, key := range []string{global, oldScoped, Key("claude_code", TenantScope("third"), fp)} {
		got, err := st.Latest(t.Context(), key)
		if err != nil || len(got) != 1 || got[0].Utilization != .99 {
			t.Fatalf("account reading for %q = %+v, %v", key, got, err)
		}
	}
	write(global, .05, now.Add(3*time.Minute))
	got, err := st.Latest(t.Context(), oldScoped)
	if err != nil || len(got) != 1 || got[0].Utilization != .05 {
		t.Fatalf("newer provider reading did not replace the old one: %+v, %v", got, err)
	}
	// Exercise the tie through both real stores, not only the fold helper.
	tied := Reading{Window: WindowSevenDay, Status: StatusRejected, ObservedAt: now.Add(4 * time.Minute), ResetsAt: now.Add(time.Hour)}
	if err := st.Record(t.Context(), global, tied); err != nil {
		t.Fatal(err)
	}
	stale := tied
	stale.ResetsAt = now.Add(-time.Second)
	if err := st.Record(t.Context(), oldScoped, stale); err != nil {
		t.Fatal(err)
	}
	for range 25 {
		got, err := st.Latest(t.Context(), global)
		if err != nil || len(got) != 1 || !got[0].ResetsAt.Equal(tied.ResetsAt) {
			t.Fatalf("equal-time refused account window is unstable: %+v, %v", got, err)
		}
	}
	if _, err := st.DeleteByFingerprint(t.Context(), fp); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Latest(t.Context(), global); err != nil || len(got) != 0 {
		t.Fatalf("explicit reset left account readings: %+v, %v", got, err)
	}
}
