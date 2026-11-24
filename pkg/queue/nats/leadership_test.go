package nats

import "testing"

// decideLeadershipHeal is the guard half of the post-election recovery
// (issue #2890): the table pins every arm in precedence order so a
// regression in the interlocks cannot silently delete a healthy
// consumer, nor leave the Oct 30 wedge shape — held work frozen while
// the stream advances — without its recreate. The inflow witness is
// what separates the wedge from a healthy busy fleet (runs executing
// hold ack-pending for minutes over a drained queue): frozen held work
// over a QUIET stream recreates nothing.
func TestDecideLeadershipHeal(t *testing.T) {
	t.Parallel()

	frozen := Sample{Pending: 1, DeliveredConsumerSeq: 7}
	work := Sample{Pending: 1, AckPending: 0, DeliveredConsumerSeq: 7}
	heldOnly := Sample{AckPending: 5, DeliveredConsumerSeq: 7}

	cases := []struct {
		name        string
		disabled    bool
		pool        bool
		onset       Sample
		fresh       Sample
		samples     int
		inflowMoved bool
		want        LeadershipRecovery
	}{
		{"off switch wins over the wedge shape", true, false, frozen, frozen, 2, true, LeadershipRecoveryDisabled},
		{"pool consumer is the reconciler's", false, true, frozen, frozen, 2, true, LeadershipRecoveryPoolOwned},
		{"off switch outranks pool", true, true, frozen, frozen, 2, true, LeadershipRecoveryDisabled},
		{"one readable sample is no movement information", false, false, frozen, frozen, 1, true, LeadershipRecoveryUnreadable},
		{"nothing readable is no information", false, false, Sample{}, Sample{}, 0, true, LeadershipRecoveryUnreadable},
		{"empty consumer has nothing to heal", false, false, Sample{}, Sample{}, 2, false, LeadershipRecoveryNoWork},
		{"fully drained mid-window reads as nothing to heal", false, false, work, Sample{Pending: 0, DeliveredConsumerSeq: 8}, 2, false, LeadershipRecoveryNoWork},
		{"deliveries moved with work still held", false, false, work, Sample{Pending: 1, DeliveredConsumerSeq: 8}, 2, false, LeadershipRecoveryResumed},
		{"movement wins over a quiet stream", false, false, work, Sample{Pending: 1, DeliveredConsumerSeq: 8}, 2, true, LeadershipRecoveryResumed},
		{"the Oct 30 shape: ack-pending frozen while the stream advances", false, false, heldOnly, heldOnly, 2, true, LeadershipRecoveryRecreated},
		{"pending work frozen with inflow recreates", false, false, work, work, 2, true, LeadershipRecoveryRecreated},
		{"the busy-fleet steady state never recreates: held work, quiet stream", false, false, heldOnly, heldOnly, 2, false, LeadershipRecoveryNoInflow},
		{"frozen pending work over a quiet stream waits for the detector", false, false, work, work, 2, false, LeadershipRecoveryNoInflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decideLeadershipHeal(tc.disabled, tc.pool, tc.onset, tc.fresh, tc.samples, tc.inflowMoved); got != tc.want {
				t.Errorf("decideLeadershipHeal() = %v, want %v", got, tc.want)
			}
		})
	}
}
