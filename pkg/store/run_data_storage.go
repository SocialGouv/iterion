package store

import (
	"context"
	"fmt"
)

// RequireRunDataStorage refuses protocols whose complete payload capability
// is not installed. Header observation deliberately does not call this guard.
// A model field or a parsed header alone is never storage capability.
func RequireRunDataStorage(protocol RunDataProtocol) error {
	switch protocol {
	case HistoricalRunData:
		return nil
	case IncarnationRunData:
		return ErrRunDataCapability
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedRunDataProtocol, protocol)
	}
}

// CheckRunDataReplacement is independent of capability admission: enabling a
// backend must not let SaveRun change ownership or manufacture a protocol-one
// birth. Historical snapshots may still omit BirthID; their CAS tail preserves
// the durable identity, as before.
func CheckRunDataReplacement(ctx context.Context, current, incoming *Run) error {
	if incoming == nil {
		return ErrInvalidRunDataIdentity
	}
	if current == nil {
		if incoming.DataProtocol != HistoricalRunData {
			return ErrRunDataCapability
		}
		return CheckMissingRunDataParent(ctx, incoming.ID)
	}
	if current.ID != incoming.ID || current.DataProtocol != incoming.DataProtocol {
		return ErrRunDataIdentityChanged
	}
	if current.DataProtocol == IncarnationRunData && (incoming.BirthID == "" || incoming.BirthID != current.BirthID) {
		return ErrRunDataIdentityChanged
	}
	return CheckRunDataBinding(ctx, RunDataHeader{RunID: current.ID, Format: ValueFormat(current.FormatVersion), Protocol: current.DataProtocol, BirthID: current.BirthID})
}
