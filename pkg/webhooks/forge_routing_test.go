package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// For reads one lane, and a name that is not a lane reads as unset: the caller
// applies its default to "", so a swapped or defaulted lane would route a
// delivery by another lane's switch.
func TestForgeRouting_For(t *testing.T) {
	r := ForgeRouting{Issues: RoutingSpine, PullRequests: RoutingShadow, Comments: RoutingLegacy}
	for _, c := range []struct {
		name    string
		routing ForgeRouting
		lane    string
		want    string
	}{
		{"issues", r, LaneIssues, RoutingSpine},
		{"pull requests", r, LanePullRequests, RoutingShadow},
		{"comments", r, LaneComments, RoutingLegacy},
		{"a name that is not a lane", r, "issue", ""},
		{"the empty name", r, "", ""},
		{"an unset lane beside a set one", ForgeRouting{PullRequests: RoutingSpine}, LaneIssues, ""},
		{"the zero routing", ForgeRouting{}, LaneIssues, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.routing.For(c.lane); got != c.want {
				t.Fatalf("%+v.For(%q) = %q, want %q", c.routing, c.lane, got, c.want)
			}
		})
	}
	if LaneIssues != "issues" || LanePullRequests != "pull_requests" || LaneComments != "comments" {
		t.Fatalf("lane names moved: %q %q %q — they are the routing API's vocabulary", LaneIssues, LanePullRequests, LaneComments)
	}
	if RoutingLegacy != "legacy" || RoutingShadow != "shadow" || RoutingSpine != "spine" {
		t.Fatalf("routing modes moved: %q %q %q — they are stored on every flipped config", RoutingLegacy, RoutingShadow, RoutingSpine)
	}
}

// The config stores keep a stored routing against a writer that says nothing
// about it, and "says nothing" is decided by the codec: MongoConfigStore.Update
// marshals the written struct and merges it over the stored document, so a
// routing field the struct leaves unset must encode NO key. The driver omits
// an `omitempty` pointer when it is nil, and an `omitempty` struct only when
// its type reports itself zero — ForgeRouting.IsZero.
//
// This runs on the real codec and needs no server.
func TestConfigBSON_UnsetRoutingFieldsEncodeNoKey(t *testing.T) {
	encode := func(t *testing.T, c Config) bson.Raw {
		t.Helper()
		raw, err := bson.Marshal(c)
		if err != nil {
			t.Fatalf("bson.Marshal: %v", err)
		}
		return raw
	}
	absent := func(t *testing.T, raw bson.Raw, key string) {
		t.Helper()
		if v, ok := bsonField(t, raw, key); ok {
			t.Errorf("a config that sets no %s still encodes the key (as %s): the store's merge would overwrite the stored value with it", key, v)
		}
	}

	t.Run("a config holding neither field", func(t *testing.T) {
		raw := encode(t, Config{ID: "wh", TenantID: "t1", Name: "unflipped"})
		absent(t, raw, "forge_routing")
		absent(t, raw, "spine_since")
	})

	t.Run("a config holding both", func(t *testing.T) {
		since := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
		raw := encode(t, Config{ID: "wh", TenantID: "t1", ForgeRouting: ForgeRouting{Issues: RoutingSpine}, SpineSince: &since})
		mode, ok := bsonField(t, raw, "forge_routing", "issues")
		if !ok {
			t.Fatal("forge_routing.issues is not encoded")
		}
		if s, isString := mode.StringValueOK(); !isString || s != RoutingSpine {
			t.Fatalf("forge_routing.issues = %s, want the string %q", mode, RoutingSpine)
		}
		// The lanes nothing was chosen for stay off the wire too.
		for _, lane := range []string{"pull_requests", "comments"} {
			if v, ok := bsonField(t, raw, "forge_routing", lane); ok {
				t.Errorf("forge_routing.%s is encoded as %s, want no key for an unset lane", lane, v)
			}
		}
		var back Config
		if err := bson.Unmarshal(raw, &back); err != nil {
			t.Fatalf("bson.Unmarshal: %v", err)
		}
		if back.ForgeRouting != (ForgeRouting{Issues: RoutingSpine}) || back.SpineSince == nil || !back.SpineSince.Equal(since) {
			t.Fatalf("round trip = routing %+v / since %v, want issues=spine / %v", back.ForgeRouting, back.SpineSince, since)
		}
	})

	// A rollback is a value: "legacy" is encoded, so it does replace a stored
	// "spine" — which the zero routing, encoding nothing, never would.
	t.Run("an explicit legacy", func(t *testing.T) {
		raw := encode(t, Config{ID: "wh", ForgeRouting: ForgeRouting{Issues: RoutingLegacy}})
		mode, ok := bsonField(t, raw, "forge_routing", "issues")
		if !ok {
			t.Fatal("an explicit legacy encodes no forge_routing.issues: a rollback would be a no-op")
		}
		if s, isString := mode.StringValueOK(); !isString || s != RoutingLegacy {
			t.Fatalf("forge_routing.issues = %s, want the string %q", mode, RoutingLegacy)
		}
	})
}

// The webhook REST answers a Config as JSON: a config no lane was ever chosen
// for answers neither routing key, so its body is the same on every
// unflipped integration, and a flipped one shows its switch.
func TestConfigJSON_UnsetRoutingFieldsEncodeNoKey(t *testing.T) {
	keys := func(t *testing.T, c Config) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		return out
	}
	unflipped := keys(t, Config{ID: "wh", TenantID: "t1"})
	for _, key := range []string{"forge_routing", "spine_since"} {
		if v, ok := unflipped[key]; ok {
			t.Errorf("a config that sets no %s answers %s:%s — every unflipped config's body would change", key, key, v)
		}
	}
	since := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	flipped := keys(t, Config{ID: "wh", ForgeRouting: ForgeRouting{Issues: RoutingSpine}, SpineSince: &since})
	if got := string(flipped["forge_routing"]); got != `{"issues":"spine"}` {
		t.Errorf("forge_routing = %s, want {\"issues\":\"spine\"}", got)
	}
	if got := string(flipped["spine_since"]); got != `"2026-11-23T09:30:00Z"` {
		t.Errorf("spine_since = %s, want the RFC 3339 instant", got)
	}
}

// Update replaces the whole document, so a routing field survives a writer
// only if the store keeps it: a Config built from a literal, or a snapshot
// that blanked the fields, holds neither, and must not revert a flip. The
// stores keep the stored ForgeRouting when the written one is zero and the
// stored SpineSince when the written one is nil; a written routing — the
// explicit "legacy" of a rollback included — replaces the stored one whole.
func TestConfigStore_RoutingFieldsSurviveAWriterThatOmitsThem(t *testing.T) {
	since := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	later := since.Add(48 * time.Hour)
	spine := ForgeRouting{Issues: RoutingSpine}

	cases := []struct {
		name        string
		stored      ForgeRouting
		storedSince *time.Time
		// write is the writer under test: it receives a fresh read and
		// returns what it hands to Update.
		write       func(cur Config) Config
		wantRouting ForgeRouting
		wantSince   *time.Time
	}{
		{
			name: "a literal holding neither field", stored: spine, storedSince: &since,
			write: func(cur Config) Config {
				return Config{ID: cur.ID, TenantID: cur.TenantID, TokenHash: cur.TokenHash, Version: cur.Version, CreatedAt: cur.CreatedAt}
			},
			wantRouting: spine, wantSince: &since,
		},
		{
			name: "a snapshot that blanked both", stored: spine, storedSince: &since,
			write: func(cur Config) Config {
				cur.ForgeRouting, cur.SpineSince = ForgeRouting{}, nil
				return cur
			},
			wantRouting: spine, wantSince: &since,
		},
		{
			name: "a read-modify-write", stored: spine, storedSince: &since,
			write:       func(cur Config) Config { return cur },
			wantRouting: spine, wantSince: &since,
		},
		{
			// The rollback of ADR-172 §8: legacy is written, SpineSince is
			// not, and legacy keeps reading the spine's claims from it.
			name: "an explicit rollback to legacy", stored: spine, storedSince: &since,
			write: func(cur Config) Config {
				cur.ForgeRouting, cur.SpineSince = ForgeRouting{Issues: RoutingLegacy}, nil
				return cur
			},
			wantRouting: ForgeRouting{Issues: RoutingLegacy}, wantSince: &since,
		},
		{
			name: "a written routing replaces the stored one whole", stored: ForgeRouting{Issues: RoutingSpine, PullRequests: RoutingShadow}, storedSince: &since,
			write: func(cur Config) Config {
				cur.ForgeRouting = ForgeRouting{Issues: RoutingLegacy}
				return cur
			},
			wantRouting: ForgeRouting{Issues: RoutingLegacy}, wantSince: &since,
		},
		{
			name: "a written instant replaces the stored one", stored: spine, storedSince: &since,
			write: func(cur Config) Config {
				cur.ForgeRouting, cur.SpineSince = ForgeRouting{}, &later
				return cur
			},
			wantRouting: spine, wantSince: &later,
		},
		{
			name: "a config no lane was chosen for stays unrouted",
			write: func(cur Config) Config {
				return Config{ID: cur.ID, TenantID: cur.TenantID, TokenHash: cur.TokenHash, Version: cur.Version, CreatedAt: cur.CreatedAt}
			},
		},
	}

	onConfigStoreTwins(t, func(t *testing.T, st ConfigStore) {
		ctx := context.Background()
		for i, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				// token_hash carries a unique index: one hash per fixture.
				id := "wh-routing-" + string(rune('a'+i))
				if err := st.Create(ctx, Config{ID: id, TenantID: "t1", TokenHash: "hash-" + id, Name: "created"}); err != nil {
					t.Fatalf("Create: %v", err)
				}
				// The flip, as the provisioner writes it: an Update carrying
				// both fields. It lands, or nothing below proves anything.
				if !c.stored.IsZero() || c.storedSince != nil {
					flip, err := st.Get(ctx, id)
					if err != nil {
						t.Fatalf("Get before the flip: %v", err)
					}
					flip.ForgeRouting, flip.SpineSince = c.stored, c.storedSince
					if err := st.Update(ctx, flip); err != nil {
						t.Fatalf("Update (the flip): %v", err)
					}
					flipped, err := st.Get(ctx, id)
					if err != nil {
						t.Fatalf("Get after the flip: %v", err)
					}
					if flipped.ForgeRouting != c.stored || !sameInstant(flipped.SpineSince, c.storedSince) {
						t.Fatalf("the flip did not land: routing %+v / since %v, want %+v / %v", flipped.ForgeRouting, flipped.SpineSince, c.stored, c.storedSince)
					}
				}

				cur, err := st.Get(ctx, id)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				written := c.write(cur)
				written.Name = "rewritten"
				if err := st.Update(ctx, written); err != nil {
					t.Fatalf("Update (the writer under test): %v", err)
				}
				got, err := st.Get(ctx, id)
				if err != nil {
					t.Fatalf("Get after the write: %v", err)
				}
				if got.Name != "rewritten" {
					t.Fatalf("the write did not land (name %q): the assertions below would pass on an untouched row", got.Name)
				}
				if got.ForgeRouting != c.wantRouting {
					t.Errorf("ForgeRouting after the write = %+v, want %+v", got.ForgeRouting, c.wantRouting)
				}
				if !sameInstant(got.SpineSince, c.wantSince) {
					t.Errorf("SpineSince after the write = %v, want %v", got.SpineSince, c.wantSince)
				}
			})
		}
	})
}

// sameInstant compares two optional instants: both unset, or both set to the
// same moment.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// bsonField walks an encoded document down path and returns the value under
// its last key, and whether every key of the path exists. It reads the bytes
// the codec produced, not a decoded struct: a key the decoder would default
// is exactly what these tests tell apart from an absent one.
func bsonField(t *testing.T, raw bson.Raw, path ...string) (bson.RawValue, bool) {
	t.Helper()
	doc := raw
	for depth, key := range path {
		elems, err := doc.Elements()
		if err != nil {
			t.Fatalf("read the encoded document at %v: %v", path[:depth], err)
		}
		var value bson.RawValue
		found := false
		for _, e := range elems {
			if e.Key() == key {
				value, found = e.Value(), true
				break
			}
		}
		if !found {
			return bson.RawValue{}, false
		}
		if depth == len(path)-1 {
			return value, true
		}
		sub, ok := value.DocumentOK()
		if !ok {
			t.Fatalf("%v: %q holds %s, not a document", path, key, value)
		}
		doc = sub
	}
	return bson.RawValue{}, false
}
