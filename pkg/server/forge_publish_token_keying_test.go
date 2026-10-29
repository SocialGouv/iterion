package server

import (
	"strings"
	"testing"
	"time"
)

// forgeGrantSentinelToken stands in for a minted bearer: a value no real
// token can take, so finding it in a key NAME proves the leak #2033 names.
const forgeGrantSentinelToken = "sentinel-forge-grant-2033"

// Item 4 of #2033: the grant was keyed `iterion:forgepub:tok:<bearer>`, so
// anything that can SCAN, MONITOR or read the slowlog read live grants from
// key names alone. The key names the SHA-256 of the bearer instead, and
// nothing in the keyspace — name or TTL index — carries it.
func TestNoValkeyKeyCarriesTheForgePublishBearer(t *testing.T) {
	mr, rdb := newTestRedis(t)
	st := newValkeyForgePublishTokenStore(rdb, nil)
	if err := st.Register(forgeGrantSentinelToken, ForgePublishGrant{TeamID: "t1", ConnectionID: "c1", Repo: "acme/widgets"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("Register stored nothing")
	}
	for _, k := range keys {
		if strings.Contains(k, forgeGrantSentinelToken) {
			t.Errorf("a key name carries the bearer: %q", k)
		}
	}
	// Not a capability loss: the grant is still found by the bearer.
	if g, ok := st.lookup(forgeGrantSentinelToken); !ok || g.Repo != "acme/widgets" {
		t.Errorf("lookup = %+v %v, want the registered grant", g, ok)
	}
	if _, ok := st.lookup("another-token"); ok {
		t.Error("a token that was never registered resolved to a grant")
	}
}

// The cutover's bridge: a grant minted by a previous build lives under the
// legacy key for up to one grant TTL. Every operation must still find it —
// a lookup that missed it would refuse a live publish, and a revoke that
// missed it would leave a withdrawn grant usable.
func TestAGrantUnderTheLegacyKeyIsStillServedAndReaped(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(t *testing.T, st *valkeyForgePublishTokenStore)
		gone bool
	}{
		{name: "lookup", act: func(t *testing.T, st *valkeyForgePublishTokenStore) {
			if g, ok := st.lookup(forgeGrantSentinelToken); !ok || g.Repo != "acme/widgets" {
				t.Errorf("lookup = %+v %v, want the legacy-keyed grant", g, ok)
			}
		}},
		{name: "update", act: func(t *testing.T, st *valkeyForgePublishTokenStore) {
			found, err := st.update(forgeGrantSentinelToken, func(g *ForgePublishGrant) { g.Bot = "review-pr" })
			if err != nil || !found {
				t.Fatalf("update = %v %v, want the legacy-keyed grant", found, err)
			}
			if g, _ := st.lookup(forgeGrantSentinelToken); g.Bot != "review-pr" {
				t.Errorf("the update did not reach the stored grant: %+v", g)
			}
		}},
		{name: "revoke", gone: true, act: func(t *testing.T, st *valkeyForgePublishTokenStore) {
			st.Revoke(forgeGrantSentinelToken)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, rdb := newTestRedis(t)
			st := newValkeyForgePublishTokenStore(rdb, nil)
			// What a pre-cutover build wrote: the bearer AS the key.
			if err := rdb.Set(t.Context(), forgePublishTokenKeyPrefix+forgeGrantSentinelToken,
				`{"team_id":"t1","connection_id":"c1","repo":"acme/widgets"}`, forgePublishDefaultTTL).Err(); err != nil {
				t.Fatalf("seed the legacy key: %v", err)
			}
			tc.act(t, st)
			if _, ok := st.lookup(forgeGrantSentinelToken); ok == tc.gone {
				t.Errorf("after %s the grant is %v, want gone=%v", tc.name, ok, tc.gone)
			}
			// The expiry of a legacy-keyed grant is brought forward too: a
			// terminal run must not leave a 9-day grant behind.
			if !tc.gone {
				st.expireIn(forgeGrantSentinelToken, time.Minute)
				if ttl := mr.TTL(forgePublishTokenKeyPrefix + forgeGrantSentinelToken); ttl > time.Minute {
					t.Errorf("the legacy key's TTL is %s, want it shortened to a minute", ttl)
				}
			}
		})
	}
}
