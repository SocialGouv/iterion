package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// routingLockPosture says whether the writing level may carry `locks:` in
// its routing block (ADR-121: a lock vetoes every setter above the level).
type routingLockPosture int

const (
	// routingLocksAllowed — the platform record and the org/team records
	// hold the lock primitive: their block may name fields they own.
	routingLocksAllowed routingLockPosture = iota
	// routingLocksRefused — a BINDING surface (a schedule row, a forge
	// integration) holds no lock primitive; accepting a lock there would
	// launder "unset" past the author's own lock.
	routingLocksRefused
)

// decodeRoutingBlock is the ONE decoder of a request's routing field
// (pkg/llmroute, ADR-121/129), shared by every write surface of an
// llmroute.Policy: the platform PATCH, the org/team PUT, the schedule
// create/PATCH and the forge-integration enable/PATCH. One decoder is how
// the surfaces keep ONE strictness, ONE validation and ONE lock policy —
// a per-surface copy is how two surfaces end up two lock policies apart.
//
// The three-way rule every binding surface shares: an ABSENT field leaves
// the stored block untouched (an edit dialog that mirrors the row back
// must not wipe what it does not know about), `null` clears it, an object
// replaces it wholesale. The bool reports that the request SPOKE — a null
// or an object — which the caller needs to tell "keep" from "clear".
//
// The decode is STRICT (DisallowUnknownFields): a camelCase typo must
// refuse, not silently store a no-op block audited as set. Locks follow
// the posture; the block is validated with llmroute.Validate.
func decodeRoutingBlock(raw json.RawMessage, locks routingLockPosture) (*llmroute.Policy, bool, *routingRequestError) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var block llmroute.Policy
	if err := dec.Decode(&block); err != nil {
		return nil, false, &routingRequestError{http.StatusBadRequest, fmt.Errorf("routing: %w", err)}
	}
	if locks == routingLocksRefused && len(block.Locks) > 0 {
		return nil, false, &routingRequestError{http.StatusUnprocessableEntity,
			fmt.Errorf("routing.locks: the binding level does not hold the lock primitive — the author's manifest and the platform record do (locks: %s)", strings.Join(block.Locks, ", "))}
	}
	if err := llmroute.Validate(block); err != nil {
		return nil, false, &routingRequestError{http.StatusBadRequest, err}
	}
	return &block, true, nil
}
