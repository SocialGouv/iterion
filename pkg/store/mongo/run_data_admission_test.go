package mongo

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Deliberately disconnected: refusing an unsupported protocol must precede
// database, blob, or lease effects, in both public creation entry points.
func TestDataProtocolAdmissionRefusesBeforeMongoIO(t *testing.T) {
	t.Parallel()
	for _, protocol := range []store.RunDataProtocol{store.IncarnationRunData, -1, 2} {
		spec := store.RunCreateSpec{ID: "r", DataProtocol: protocol}
		want := store.ErrUnsupportedRunDataProtocol
		if protocol == store.IncarnationRunData {
			want = store.ErrRunDataCapability
		}
		s := &Store{}
		if _, err := s.CreateRunWithValueFormat(t.Context(), spec); !errors.Is(err, want) {
			t.Fatalf("direct protocol %d: %v", protocol, err)
		}
		reservation := &runBirth{id: spec.ID, store: s}
		if _, err := reservation.Publish(t.Context(), spec); !errors.Is(err, want) {
			t.Fatalf("reserved protocol %d: %v", protocol, err)
		}
	}
}
