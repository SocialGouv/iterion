package runstream

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrSourceClosed ends a subscription because its source was closed, not
// because its run ended: the stream is over without a terminal completion.
var ErrSourceClosed = errors.New("runstream: source closed")

// IsPermanentError identifies stream refusals that cannot be retried against the
// same original observation, run metadata that does not decode included, and
// the closure of the subscription's source.
// Cancellation and transient warnings alone are not permanent.
func IsPermanentError(err error) bool {
	return errors.Is(err, ErrSourceClosed) || errors.Is(err, store.ErrRunNotFound) || store.IsValueFormatRefusal(err) || errors.Is(err, store.ErrRunDataIdentityChanged) || errors.Is(err, store.ErrRunDataBindingRequired) || errors.Is(err, store.ErrInvalidRunDataIdentity) || errors.Is(err, store.ErrRunDataCapability) || errors.Is(err, store.ErrUnsupportedRunDataProtocol) || errors.Is(err, store.ErrRunDeleted) || errors.Is(err, store.ErrEventsCorrupted) || errors.Is(err, store.ErrRunMetadataUndecodable)
}
