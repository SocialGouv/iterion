package cloudpublisher

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
)

// TestPublishInstant_neverInsideTheGrantsMillisecond: a publication is
// never inside the millisecond of the lease its grant opened — the spend
// report's pin compares the pair strictly at that precision, and a tie
// would hand the attempt's own report to another lease.
func TestPublishInstant_neverInsideTheGrantsMillisecond(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 500*int(time.Microsecond), time.UTC)
	// The lease the grant opened was floored into a LATER millisecond than
	// the marker (a fast chain of attempts): the binding constraint.
	grant := &credpool.Grant{AcquiredAt: now.Add(2 * time.Millisecond)}
	marker := now
	got := publishInstant(now, marker, grant)
	if !got.Truncate(time.Millisecond).After(grant.AcquiredAt.Truncate(time.Millisecond)) {
		t.Fatalf("the publication (%v) sits inside the grant's lease millisecond (%v): the pin would meet a tie", got, grant.AcquiredAt)
	}
	// Without a grant, the marker's own floor still applies.
	if got = publishInstant(now, marker, nil); !got.Truncate(time.Millisecond).After(marker.Truncate(time.Millisecond)) {
		t.Fatalf("the publication (%v) sits inside the marker's millisecond (%v)", got, marker)
	}
}
