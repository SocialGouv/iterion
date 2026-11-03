package storekit

import (
	"context"
	"fmt"
	"maps"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// PinField names one credential-pin map of a pin-bearing row: the
// subdocument's Mongo path and the accessor that reads it off a decoded row.
type PinField[T any] struct {
	Path string
	Of   func(T) map[string]string
}

// MongoPinClear is the Mongo half of a pin-bearing store's
// ClearCredentialPins — the one body the two homes of a credential pin run
// (webhooks.MongoConfigStore, trigger.MongoSubscriptionStore), as
// DropStringMapValues is their memory half. The stores keep their own rows,
// collections and error vocabulary; the clear itself, its rewrite arm and
// its retry bound live here once, so a fix to one cannot drift from the
// other.
type MongoPinClear[T any] struct {
	// Coll holds the rows.
	Coll *mongo.Collection
	// Get reads one row as the store decodes it, answering the store's own
	// not-found sentinel for an absent row.
	Get func(ctx context.Context, id string) (T, error)
	// Version reads the row's optimistic-concurrency stamp.
	Version func(T) int64
	// Fields are the row's pin maps.
	Fields []PinField[T]
	// Op prefixes every wrapped failure ("webhooks: clear config credential
	// pins").
	Op string
}

// Clear removes every pin entry of the row whose value names recordID and
// reports whether it removed any. The entry names come from a read taken
// HERE, not from the caller's listing copy, and each removal is one $unset
// whose filter still matches the dead value, so a pin edit, a health stamp
// or a second record's sweep landing in between survives untouched. Each
// real removal $inc's the row's version, so a full-document editor holding a
// pre-sweep snapshot conflicts and re-reads instead of writing the swept pin
// back. A zero match on an entry means it moved or the row died between the
// read and the write — the stored state is already the wanted one, so
// nothing to clear is not a failure. Addressability is two arms, not an
// assumption: an entry whose name no Mongo path can address
// (PinNameAddressable) clears by rewriting its subdocument under a version
// CAS (clearUnaddressable), never by a $unset that would match nothing.
func (c MongoPinClear[T]) Clear(ctx context.Context, id, recordID string) (bool, error) {
	row, err := c.Get(ctx, id)
	if err != nil {
		return false, err
	}
	cleared := false
	for _, field := range c.Fields {
		for name, val := range field.Of(row) {
			if val != recordID {
				continue
			}
			if !PinNameAddressable(name) {
				dropped, err := c.clearUnaddressable(ctx, id, field, name, recordID)
				if err != nil {
					return false, err
				}
				cleared = cleared || dropped
				continue
			}
			res, err := c.Coll.UpdateOne(ctx,
				bson.M{"_id": id, field.Path + "." + name: recordID},
				bson.M{"$unset": bson.M{field.Path + "." + name: ""}, "$inc": bson.M{"version": 1}})
			if err != nil {
				return false, fmt.Errorf("%s: %w", c.Op, err)
			}
			cleared = cleared || res.MatchedCount > 0
		}
	}
	return cleared, nil
}

// pinClearCASAttempts bounds the rewrite arm's read-CAS retries. Each
// attempt re-reads, so a writer that moved the row between the read and the
// CAS wins it and the retry judges the row as the store now holds it — a
// stale map is never written back. Exhaustion needs a writer moving the row
// on every attempt of a rare legacy arm; it refuses explicitly rather than
// reporting a clear that did not happen.
const pinClearCASAttempts = 5

// clearUnaddressable drops an entry whose stored name a Mongo path cannot
// address (a legacy '.'/'$'/empty name — the routes' validators refuse new
// ones). The rewrite is value-guarded: the entry must still name recordID on
// the row as THIS attempt read it, so an entry that moved or was re-pointed
// between the caller's listing and here is already the wanted state, not a
// failure. The CAS is VersionCAS, the fence the stores' Update commits
// under: the legacy names this arm exists for live on rows written before
// the version field did, and a filter on the bare 0 would never match one —
// the clear would exhaust its attempts and fail every delete's sweep of that
// row.
func (c MongoPinClear[T]) clearUnaddressable(ctx context.Context, id string, field PinField[T], name, recordID string) (bool, error) {
	for range pinClearCASAttempts {
		row, err := c.Get(ctx, id)
		if err != nil {
			return false, err
		}
		pins := field.Of(row)
		if cur, ok := pins[name]; !ok || cur != recordID {
			return false, nil
		}
		wanted := maps.Clone(pins)
		delete(wanted, name)
		res, err := c.Coll.UpdateOne(ctx, VersionCAS(id, c.Version(row)),
			bson.M{"$set": bson.M{field.Path: wanted}, "$inc": bson.M{"version": 1}})
		if err != nil {
			return false, fmt.Errorf("%s: %w", c.Op, err)
		}
		if res.MatchedCount > 0 {
			return true, nil
		}
	}
	return false, fmt.Errorf("%s: the subdocument rewrite never landed on %s after %d attempts — the row keeps moving", c.Op, id, pinClearCASAttempts)
}

// VersionCAS is the filter a write commits a row under only at the version
// its read carried — the optimistic-concurrency fence of the pin-bearing
// rows (webhooks.Config, trigger.Subscription), one definition for their
// full-document Update and for the clear's rewrite arm. A version of 0
// matches a row that predates the field as well: such a row decodes as 0,
// and a filter on the bare 0 would never match it, so its first versioned
// write would conflict forever. The returned map is the caller's to extend
// (the webhooks Update adds its provision_frozen fence).
func VersionCAS(id string, version int64) bson.M {
	if version == 0 {
		return bson.M{"_id": id, "$or": bson.A{bson.M{"version": 0}, bson.M{"version": bson.M{"$exists": false}}}}
	}
	return bson.M{"_id": id, "version": version}
}
