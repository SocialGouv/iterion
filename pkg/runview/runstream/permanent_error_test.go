package runstream

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPermanentStreamErrorClassification(t *testing.T) {
	t.Parallel()
	causes := []error{store.ErrRunNotFound, store.ErrValueFormatChanged, store.ErrUnsupportedValueFormat, store.ErrInvalidValueFormat, store.ErrValueFormatCapability, store.ErrRunDataIdentityChanged, store.ErrRunDataBindingRequired, store.ErrInvalidRunDataIdentity, store.ErrRunDataCapability, store.ErrUnsupportedRunDataProtocol, store.ErrRunDeleted, store.ErrEventsCorrupted}
	for _, cause := range causes {
		for _, err := range []error{cause, fmt.Errorf("wrapped: %w", cause), errors.Join(context.Canceled, cause)} {
			if !IsPermanentError(err) {
				t.Errorf("permanent cause lost: %v", err)
			}
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("reconnect transport"), ErrLogsUnsupported, errors.Join(context.Canceled, errors.New("IO"))} {
		if IsPermanentError(err) {
			t.Errorf("non-permanent classified: %v", err)
		}
	}
}
