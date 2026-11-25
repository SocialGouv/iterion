package trigger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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

// DerivedInvocationKey is the identity of invocations[index] within its
// bot's manifest, intrinsic to what the invocation says (a reorder or an
// insertion of another invocation never moves it): the package trigger id
// for a connector invocation, the normalized cron for a schedule, the
// canonical interval for a keepalive, a digest of the board block, and for
// a forge invocation its event, its actions and a digest of its labels. ""
// derives no subscription (the builders refuse it). Two invocations of one
// kind sharing a key derive the same subscription: the manifest decoder
// refuses the pair, except for two forge invocations, where the first one
// derives and the second is warned (bundle.ForgeDuplicateKeyWarnings).
func DerivedInvocationKey(invs []bundle.Invocation, index int) string {
	return bundle.InvocationKey(invs, index)
}

// BotHomeOrigin is the Origin of a subscription the operator enabled from
// the bot home (the server's from-invocation route).
const BotHomeOrigin = "bot-home"

// Tombstone records that an operator deleted a derived subscription: no
// reconciliation re-creates the row until an operator re-enables it.
type Tombstone struct {
	ID        string    `json:"id" bson:"_id"`
	TenantID  string    `json:"tenant_id" bson:"tenant_id"`
	DeletedAt time.Time `json:"deleted_at" bson:"deleted_at"`
}
