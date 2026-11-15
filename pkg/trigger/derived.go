package trigger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// DerivedIDPrefix marks the id of a subscription derived from a bot's
// manifest invocation: the id is computed from what the subscription is
// (DerivedSubscriptionID), never drawn at random, so every reconciliation
// and every replica names the same row.
const DerivedIDPrefix = "drv-"

// DerivedSubscriptionID is the stable id of the subscription a bot's
// invocation derives in a tenant: the tenant, the bot, the invocation kind,
// its key (DerivedInvocationKey) and, for a connector invocation, the
// connection it is bound to. Each field is length-prefixed before hashing,
// so no two field lists share an id by concatenation.
func DerivedSubscriptionID(tenantID, botID string, kind bundle.InvocationKind, key, connectionID string) string {
	h := sha256.New()
	var n [8]byte
	for _, f := range []string{tenantID, botID, string(kind), key, connectionID} {
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return DerivedIDPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// IsDerivedID reports whether id names a manifest-derived subscription.
func IsDerivedID(id string) bool { return strings.HasPrefix(id, DerivedIDPrefix) }

// DerivedInvocationKey is the key of invocations[index] within its bot's
// manifest: the package trigger id for a connector invocation, else the
// invocation's ordinal among the invocations of its kind — so reordering two
// kinds keeps every id, and two invocations of one kind keep theirs apart.
func DerivedInvocationKey(invs []bundle.Invocation, index int) string {
	inv := invs[index]
	if inv.Kind == bundle.InvocationKindConnector && inv.Connector != nil {
		return inv.Connector.Trigger
	}
	ordinal := 0
	for _, other := range invs[:index] {
		if other.Kind == inv.Kind {
			ordinal++
		}
	}
	return strconv.Itoa(ordinal)
}

// Tombstone records that an operator deleted a derived subscription: no
// reconciliation re-creates the row until an operator re-enables it.
type Tombstone struct {
	ID        string    `json:"id" bson:"_id"`
	TenantID  string    `json:"tenant_id" bson:"tenant_id"`
	DeletedAt time.Time `json:"deleted_at" bson:"deleted_at"`
}
