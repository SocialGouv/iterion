package runstream

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
)

// IsPermanentError identifies stream refusals that cannot be retried against the
// same original observation. Cancellation and transient warnings alone are not permanent.
func IsPermanentError(err error) bool {
	return errors.Is(err, store.ErrRunNotFound) || store.IsValueFormatRefusal(err) || errors.Is(err, store.ErrRunDataIdentityChanged) || errors.Is(err, store.ErrRunDataBindingRequired) || errors.Is(err, store.ErrInvalidRunDataIdentity) || errors.Is(err, store.ErrRunDataCapability) || errors.Is(err, store.ErrUnsupportedRunDataProtocol) || errors.Is(err, store.ErrRunDeleted) || errors.Is(err, store.ErrEventsCorrupted)
}
