package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The capacity transit (ADR-167 C1) and the ledger's admission identity, on
// both twins: the filesystem rows read the Go predicates, the Mongo rows the
// query filters and the conditional writes that mirror them.

// transitRun writes a run as a redelivery leaves it: failed_resumable with a
// stamp, then the runner's continuation promote stating redelivery_pending
// until `until` — the writer the transit has in production.
func transitRun(t *testing.T, s store.RunStore, id string, stamp store.RunCredStamp, until time.Time) {
	t.Helper()
	capacityStampedRun(t, s, id, store.RunStatusFailedResumable, stamp)
	promoteTransit(t, s, id, until)
}

// promoteTransit is the runner's promote of a failed_resumable run.
func promoteTransit(t *testing.T, s store.RunStore, id string, until time.Time) {
	t.Helper()
	promoteTransitIn(t, s, testCtx(), id, until)
}

// promoteTransitIn is promoteTransit under the identity of the run's tenant.
func promoteTransitIn(t *testing.T, s store.RunStore, ctx context.Context, id string, until time.Time) {
	t.Helper()
	changed, err := s.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, "",
		store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending, ContinuationUntil: until},
		[]store.RunStatus{store.RunStatusFailedResumable})
	if err != nil || !changed {
		t.Fatalf("promote %s to redelivery_pending until %s: changed=%v err=%v", id, until, changed, err)
	}
}

func loadForTransit(t *testing.T, s store.RunStore, id string) *store.Run {
	t.Helper()
	r, err := s.LoadRun(testCtx(), id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	return r
}

// testCapacityTransitHolder pins the transit arm of the holder predicate
// through every reading the ledger makes: the count (by fingerprint and by
// subject), the refused reservation (the transit run adopted as a holder) and
// the alive listing. A transit whose instant has passed holds nothing; a
// continuation other than redelivery_pending — stated by its writer, which
// removes the instant — holds nothing; a paused run holds nothing.
func testCapacityTransitHolder(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	ledger := store.AsCredSlotStore(s)
	lister := store.AsAliveCapacityLister(s)
	if ledger == nil || lister == nil {
		t.Fatal("the store must host the ledger and the alive listing")
	}
	// One key and one gateway per case: each case's run is the only one
	// stamped with either.
	stamp := func(fp string) store.RunCredStamp {
		return store.RunCredStamp{Fingerprints: []string{fp}, CapacitySubjects: []string{store.GatewayCapacitySubject(fp + ".example")}}
	}
	now := time.Now().UTC()
	for _, c := range []struct {
		name  string
		seed  func(id, fp string)
		holds bool
	}{
		{"fresh transit holds", func(id, fp string) { transitRun(t, s, id, stamp(fp), now.Add(time.Minute)) }, true},
		{"expired transit holds nothing", func(id, fp string) { transitRun(t, s, id, stamp(fp), now.Add(-time.Minute)) }, false},
		{"an armed retry ends the hold", func(id, fp string) {
			transitRun(t, s, id, stamp(fp), now.Add(time.Minute))
			if _, err := s.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, "",
				store.RunOutcomeMeta{Continuation: store.ContinuationRetryArmed}, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
				t.Fatalf("arm %s: %v", id, err)
			}
		}, false},
		{"a final continuation ends the hold", func(id, fp string) {
			transitRun(t, s, id, stamp(fp), now.Add(time.Minute))
			if _, err := s.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, "",
				store.RunOutcomeMeta{Continuation: store.ContinuationFinal}, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
				t.Fatalf("finalize %s: %v", id, err)
			}
		}, false},
		{"redelivery pending with no instant holds nothing", func(id, fp string) {
			capacityStampedRun(t, s, id, store.RunStatusFailedResumable, stamp(fp))
			if _, err := s.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, "",
				store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending}, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
				t.Fatalf("promote %s without an instant: %v", id, err)
			}
		}, false},
		{"paused operator holds nothing", func(id, fp string) { capacityStampedRun(t, s, id, store.RunStatusPausedOperator, stamp(fp)) }, false},
		{"paused waiting human holds nothing", func(id, fp string) {
			capacityStampedRun(t, s, id, store.RunStatusPausedWaitingHuman, stamp(fp))
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := "transit_" + sanitizeID(c.name)
			fp := "fp-" + id
			subject := store.GatewayCapacitySubject(fp + ".example")
			c.seed(id, fp)
			want := 0
			if c.holds {
				want = 1
			}
			if n, err := s.CountAliveRunsWithCredFingerprint(ctx, fp, ""); err != nil || n != want {
				t.Fatalf("alive(%s) = %d/%v, want %d", fp, n, err, want)
			}
			if n, err := s.CountAliveRunsWithCredFingerprint(ctx, subject, ""); err != nil || n != want {
				t.Fatalf("alive(%s) = %d/%v, want %d", subject, n, err, want)
			}
			listed := false
			list, err := lister.ListAliveRunsForCapacity(ctx)
			if err != nil {
				t.Fatalf("ListAliveRunsForCapacity: %v", err)
			}
			for _, r := range list {
				listed = listed || r.ID == id
			}
			if listed != c.holds {
				t.Fatalf("listed=%v, want %v", listed, c.holds)
			}
			// A newcomer on a key of one place: refused with the run adopted
			// when it holds, granted alone when it does not.
			newcomer := id + "_newcomer"
			capacityStampedRun(t, s, newcomer, store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{fp}})
			adm, err := ledger.ReserveCredSlot(ctx, fp, newcomer, 1)
			if err != nil {
				t.Fatalf("ReserveCredSlot: %v", err)
			}
			if adm.Granted == c.holds {
				t.Fatalf("newcomer admission %+v, want granted=%v", adm, !c.holds)
			}
		})
	}
}

// sanitizeID keeps a subtest name usable as a run id.
func sanitizeID(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// testCapacityTransitIsAdopted pins C1 literal: a run in transit that the
// ledger's row does not name — admitted while the key had no cap — is adopted
// by the next admission and counted, so a cap set while runs are in transit
// is not exceeded.
func testCapacityTransitIsAdopted(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	ledger := store.AsCredSlotStore(s)
	if ledger == nil {
		t.Fatal("the store must host the ledger")
	}
	const fp = "fp-transit-adopted"
	capacityStampedRun(t, s, "adopt_running", store.RunStatusRunning, store.RunCredStamp{Fingerprints: []string{fp}})
	transitRun(t, s, "adopt_transit", store.RunCredStamp{Fingerprints: []string{fp}}, time.Now().Add(time.Minute))
	capacityStampedRun(t, s, "adopt_newcomer", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{fp}})
	adm, err := ledger.ReserveCredSlot(ctx, fp, "adopt_newcomer", 2)
	if err != nil {
		t.Fatalf("ReserveCredSlot: %v", err)
	}
	if adm.Granted || adm.Held != 2 {
		t.Fatalf("admission on a cap of 2 set over a running and a transit run: %+v, want refused with both adopted", adm)
	}
	// The adopted transit run's entry carries no admission identity: its
	// own re-assertion with any token is not held.
	re, err := ledger.ReassertCredSlot(ctx, fp, "adopt_transit", time.Now().UTC().Truncate(time.Millisecond))
	if err != nil || re.Held {
		t.Fatalf("re-assertion of an adopted entry: %+v %v, want not held", re, err)
	}
}

// testReassertCredSlot pins store.CredSlotStore.ReassertCredSlot: it moves
// only the entry its admission wrote, never creates one, never checks the cap.
func testReassertCredSlot(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	ledger := store.AsCredSlotStore(s)
	if ledger == nil {
		t.Fatal("the store must host the ledger")
	}
	const fp = "fp-reassert"
	capacityStampedRun(t, s, "re_a", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{fp}})
	adm, err := ledger.ReserveCredSlot(ctx, fp, "re_a", 2)
	if err != nil || !adm.Granted {
		t.Fatalf("admission: %+v %v", adm, err)
	}

	t.Run("a held entry moves its instant and keeps its identity", func(t *testing.T) {
		re, err := ledger.ReassertCredSlot(ctx, fp, "re_a", adm.At)
		if err != nil || !re.Held || !re.At.After(adm.At) {
			t.Fatalf("re-assertion: %+v %v, want held at an instant after %s", re, err, adm.At)
		}
		// The identity stays: a second re-assertion by the same token holds.
		again, err := ledger.ReassertCredSlot(ctx, fp, "re_a", adm.At)
		if err != nil || !again.Held || !again.At.After(re.At) {
			t.Fatalf("second re-assertion: %+v %v, want held after %s", again, err, re.At)
		}
		// A release by the admission's own instant misses the moved entry;
		// one by the newest instant pulls it.
		if err := ledger.ReleaseCredSlot(ctx, fp, "re_a", adm.At); err != nil {
			t.Fatalf("release by the old instant: %v", err)
		}
		if still, err := ledger.ReassertCredSlot(ctx, fp, "re_a", adm.At); err != nil || !still.Held {
			t.Fatalf("after a release by the old instant: %+v %v, want the entry still held", still, err)
		}
		last, err := ledger.ReassertCredSlot(ctx, fp, "re_a", adm.At)
		if err != nil || !last.Held {
			t.Fatalf("re-assertion: %+v %v", last, err)
		}
		if err := ledger.ReleaseCredSlot(ctx, fp, "re_a", last.At); err != nil {
			t.Fatalf("release by the newest instant: %v", err)
		}
		if gone, err := ledger.ReassertCredSlot(ctx, fp, "re_a", adm.At); err != nil || gone.Held {
			t.Fatalf("after a release by the newest instant: %+v %v, want not held", gone, err)
		}
	})

	t.Run("a token from before an in-place re-admission is not held", func(t *testing.T) {
		first, err := ledger.ReserveCredSlot(ctx, fp, "re_a", 2)
		if err != nil || !first.Granted {
			t.Fatalf("admission: %+v %v", first, err)
		}
		second, err := ledger.ReserveCredSlot(ctx, fp, "re_a", 2)
		if err != nil || !second.Granted || !second.At.After(first.At) {
			t.Fatalf("re-admission in place: %+v %v", second, err)
		}
		if re, err := ledger.ReassertCredSlot(ctx, fp, "re_a", first.At); err != nil || re.Held {
			t.Fatalf("re-assertion by the first admission's token: %+v %v, want not held — a later admission wrote the entry", re, err)
		}
		if re, err := ledger.ReassertCredSlot(ctx, fp, "re_a", second.At); err != nil || !re.Held {
			t.Fatalf("re-assertion by the re-admission's token: %+v %v, want held", re, err)
		}
	})

	t.Run("no row and no entry are not held, and nothing is created", func(t *testing.T) {
		if re, err := ledger.ReassertCredSlot(ctx, "fp-reassert-no-row", "re_a", adm.At); err != nil || re.Held {
			t.Fatalf("no row: %+v %v, want not held", re, err)
		}
		// Had the call created a row naming re_a, the run stamped on that
		// key would be one holder and a cap of 1 would refuse a newcomer.
		capacityStampedRun(t, s, "re_newcomer", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{"fp-reassert-no-row"}})
		if got, err := ledger.ReserveCredSlot(ctx, "fp-reassert-no-row", "re_newcomer", 1); err != nil || !got.Granted || got.Held != 1 {
			t.Fatalf("admission after a re-assertion on no row: %+v %v, want granted alone", got, err)
		}
		if re, err := ledger.ReassertCredSlot(ctx, "fp-reassert-no-row", "re_a", adm.At); err != nil || re.Held {
			t.Fatalf("no entry: %+v %v, want not held", re, err)
		}
	})

	t.Run("a cap lowered below the holders does not refuse", func(t *testing.T) {
		const low = "fp-reassert-low"
		capacityStampedRun(t, s, "re_low_a", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{low}})
		capacityStampedRun(t, s, "re_low_b", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{low}})
		a, err := ledger.ReserveCredSlot(ctx, low, "re_low_a", 2)
		if err != nil || !a.Granted {
			t.Fatalf("admission a: %+v %v", a, err)
		}
		if b, err := ledger.ReserveCredSlot(ctx, low, "re_low_b", 2); err != nil || !b.Granted {
			t.Fatalf("admission b: %+v %v", b, err)
		}
		// The cap is now 1 (a re-admission of a at max 1 is refused); the
		// re-assertion checks no cap.
		if re, err := ledger.ReassertCredSlot(ctx, low, "re_low_a", a.At); err != nil || !re.Held {
			t.Fatalf("re-assertion over a lowered cap: %+v %v, want held", re, err)
		}
	})

	t.Run("empty or zero arguments are errors", func(t *testing.T) {
		if _, err := ledger.ReassertCredSlot(ctx, "", "re_a", adm.At); err == nil {
			t.Fatal("empty fingerprint accepted")
		}
		if _, err := ledger.ReassertCredSlot(ctx, fp, "", adm.At); err == nil {
			t.Fatal("empty run id accepted")
		}
		if _, err := ledger.ReassertCredSlot(ctx, fp, "re_a", time.Time{}); err == nil {
			t.Fatal("zero token accepted")
		}
	})
}

// testReserveWritesAdmittedAt pins the admission identity each kind of entry
// carries, through the one reader of it: a pushed entry and an in-place
// re-admission are re-asserted by their admission's At; an adopted entry by
// no token.
func testReserveWritesAdmittedAt(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	ledger := store.AsCredSlotStore(s)
	if ledger == nil {
		t.Fatal("the store must host the ledger")
	}
	const fp = "fp-admitted-at"
	capacityStampedRun(t, s, "aa_pushed", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{fp}})
	pushed, err := ledger.ReserveCredSlot(ctx, fp, "aa_pushed", 3)
	if err != nil || !pushed.Granted {
		t.Fatalf("push: %+v %v", pushed, err)
	}
	if re, err := ledger.ReassertCredSlot(ctx, fp, "aa_pushed", pushed.At); err != nil || !re.Held {
		t.Fatalf("pushed entry by its admission's At: %+v %v, want held", re, err)
	}
	readmitted, err := ledger.ReserveCredSlot(ctx, fp, "aa_pushed", 3)
	if err != nil || !readmitted.Granted {
		t.Fatalf("re-admission: %+v %v", readmitted, err)
	}
	if re, err := ledger.ReassertCredSlot(ctx, fp, "aa_pushed", readmitted.At); err != nil || !re.Held {
		t.Fatalf("re-admitted entry by its re-admission's At: %+v %v, want held", re, err)
	}
	capacityStampedRun(t, s, "aa_adopted", store.RunStatusRunning, store.RunCredStamp{Fingerprints: []string{fp}})
	capacityStampedRun(t, s, "aa_asker", store.RunStatusPausedOperator, store.RunCredStamp{Fingerprints: []string{fp}})
	if _, err := ledger.ReserveCredSlot(ctx, fp, "aa_asker", 3); err != nil {
		t.Fatalf("an admission that adopts: %v", err)
	}
	for _, token := range []time.Time{pushed.At, readmitted.At, time.Now().UTC().Truncate(time.Millisecond)} {
		if re, err := ledger.ReassertCredSlot(ctx, fp, "aa_adopted", token); err != nil || re.Held {
			t.Fatalf("adopted entry by token %s: %+v %v, want not held — an adoption writes no identity", token, re, err)
		}
	}
}

// testCapacityTransitBeginEnd pins store.CapacityTransitStore's two CAS.
func testCapacityTransitBeginEnd(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	transit := store.AsCapacityTransitStore(s)
	if transit == nil {
		t.Fatal("the store must host the capacity transit (store.CapacityTransitStore)")
	}
	until := store.TransitInstant(time.Now().Add(5 * time.Minute))

	for _, prior := range []store.ContinuationState{"", store.ContinuationFinal, store.ContinuationRedeliveryPending} {
		t.Run("begin from "+string(prior)+" returns it", func(t *testing.T) {
			id := "begin_from_" + sanitizeID(string(prior))
			capacityStampedRun(t, s, id, store.RunStatusFailedResumable, store.RunCredStamp{})
			if prior != "" {
				meta := store.RunOutcomeMeta{Continuation: prior}
				if prior == store.ContinuationRedeliveryPending {
					meta.ContinuationUntil = time.Now().Add(time.Minute)
				}
				if _, err := s.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, "", meta, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
					t.Fatalf("state %s: %v", prior, err)
				}
			}
			start, err := transit.BeginCapacityTransit(ctx, id, store.CapacityTransit{Until: until})
			if err != nil || !start.Started || start.Prior != prior {
				t.Fatalf("begin: %+v %v, want started with prior %q", start, err, prior)
			}
			r := loadForTransit(t, s, id)
			if !r.InCapacityTransit(time.Now()) || !r.ContinuationUntil.Equal(until) {
				t.Fatalf("after begin: continuation=%q until=%v, want redelivery_pending until %s", r.ContinuationState, r.ContinuationUntil, until)
			}
			// The end restores what the begin replaced.
			ended, err := transit.EndCapacityTransit(ctx, id, until, start.Prior)
			if err != nil || !ended {
				t.Fatalf("end: %v %v", ended, err)
			}
			r = loadForTransit(t, s, id)
			if r.ContinuationState != prior || r.ContinuationUntil != nil {
				t.Fatalf("after end: continuation=%q until=%v, want %q and no instant", r.ContinuationState, r.ContinuationUntil, prior)
			}
		})
	}

	t.Run("an armed retry is not taken over", func(t *testing.T) {
		capacityStampedRun(t, s, "begin_armed", store.RunStatusFailedResumable, store.RunCredStamp{})
		if _, err := s.UpdateRunOutcome(ctx, "begin_armed", store.RunStatusFailedResumable, "",
			store.RunOutcomeMeta{Continuation: store.ContinuationRetryArmed}, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
			t.Fatalf("arm: %v", err)
		}
		before := loadForTransit(t, s, "begin_armed")
		start, err := transit.BeginCapacityTransit(ctx, "begin_armed", store.CapacityTransit{Until: until})
		if err != nil || start.Started {
			t.Fatalf("begin on retry_armed: %+v %v, want not started", start, err)
		}
		after := loadForTransit(t, s, "begin_armed")
		if after.ContinuationState != store.ContinuationRetryArmed || after.ContinuationUntil != nil || after.CASVersion != before.CASVersion {
			t.Fatalf("retry_armed run written: continuation=%q until=%v version %d→%d", after.ContinuationState, after.ContinuationUntil, before.CASVersion, after.CASVersion)
		}
	})

	t.Run("a refresh names the transit it expects", func(t *testing.T) {
		capacityStampedRun(t, s, "begin_expect", store.RunStatusFailedResumable, store.RunCredStamp{})
		if start, err := transit.BeginCapacityTransit(ctx, "begin_expect", store.CapacityTransit{Until: until}); err != nil || !start.Started {
			t.Fatalf("begin: %+v %v", start, err)
		}
		later := until.Add(time.Minute)
		if start, err := transit.BeginCapacityTransit(ctx, "begin_expect", store.CapacityTransit{Until: later, ExpectUntil: until.Add(time.Second)}); err != nil || start.Started {
			t.Fatalf("refresh expecting another transit: %+v %v, want not started", start, err)
		}
		if start, err := transit.BeginCapacityTransit(ctx, "begin_expect", store.CapacityTransit{Until: later, ExpectUntil: until}); err != nil || !start.Started || start.Prior != store.ContinuationRedeliveryPending {
			t.Fatalf("refresh expecting its own transit: %+v %v, want started", start, err)
		}
		if r := loadForTransit(t, s, "begin_expect"); !r.ContinuationUntil.Equal(later) {
			t.Fatalf("refreshed until = %v, want %s", r.ContinuationUntil, later)
		}
		if ended, err := transit.EndCapacityTransit(ctx, "begin_expect", until, ""); err != nil || ended {
			t.Fatalf("end with a foreign until: %v %v, want not ended", ended, err)
		}
	})

	t.Run("an attempt fence names the attempt", func(t *testing.T) {
		capacityStampedRun(t, s, "begin_fence", store.RunStatusFailedResumable, store.RunCredStamp{})
		other := store.QueuedFlipAt(time.Now().Add(-time.Hour))
		if start, err := transit.BeginCapacityTransit(ctx, "begin_fence", store.CapacityTransit{Until: until, ExactQueuedAt: other}); err != nil || start.Started {
			t.Fatalf("begin fenced to another attempt: %+v %v, want not started", start, err)
		}
	})

	t.Run("an end after a claim does nothing", func(t *testing.T) {
		capacityStampedRun(t, s, "end_claimed", store.RunStatusFailedResumable, store.RunCredStamp{})
		if start, err := transit.BeginCapacityTransit(ctx, "end_claimed", store.CapacityTransit{Until: until}); err != nil || !start.Started {
			t.Fatalf("begin: %+v %v", start, err)
		}
		if ok, err := s.UpdateRunStatusIf(ctx, "end_claimed", store.RunStatusRunning, "", []store.RunStatus{store.RunStatusFailedResumable}); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if ended, err := transit.EndCapacityTransit(ctx, "end_claimed", until, store.ContinuationFinal); err != nil || ended {
			t.Fatalf("end after the claim: %v %v, want not ended", ended, err)
		}
		if r := loadForTransit(t, s, "end_claimed"); r.Status != store.RunStatusRunning || r.ContinuationState != "" || r.ContinuationUntil != nil {
			t.Fatalf("claimed run: status=%s continuation=%q until=%v, want running with neither", r.Status, r.ContinuationState, r.ContinuationUntil)
		}
	})

	t.Run("a missing run is not started", func(t *testing.T) {
		if start, err := transit.BeginCapacityTransit(ctx, "begin_missing", store.CapacityTransit{Until: until}); err != nil || start.Started {
			t.Fatalf("begin on no run: %+v %v, want not started", start, err)
		}
	})
}

// testContinuationUntilFollowsTheWriters pins that the transit instant moves
// with its continuation through every writer that is not the transit's own:
// a transition that states nothing clears it, a full-document save from a
// stale copy keeps it, a resume's flip and its revert put it back.
func testContinuationUntilFollowsTheWriters(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	until := store.TransitInstant(time.Now().Add(5 * time.Minute))

	t.Run("a status change with no statement clears it", func(t *testing.T) {
		transitRun(t, s, "follow_change", store.RunCredStamp{}, until)
		if _, err := s.UpdateRunOutcome(ctx, "follow_change", store.RunStatusPausedOperator, "", store.RunOutcomeMeta{}, nil); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if r := loadForTransit(t, s, "follow_change"); r.ContinuationState != "" || r.ContinuationUntil != nil {
			t.Fatalf("after a transition: continuation=%q until=%v, want neither", r.ContinuationState, r.ContinuationUntil)
		}
	})

	t.Run("a same-status rewrite with no statement keeps it", func(t *testing.T) {
		transitRun(t, s, "follow_same", store.RunCredStamp{}, until)
		if _, err := s.UpdateRunOutcome(ctx, "follow_same", store.RunStatusFailedResumable, "drained again", store.RunOutcomeMeta{}, nil); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if r := loadForTransit(t, s, "follow_same"); !r.InCapacityTransit(time.Now()) || !r.ContinuationUntil.Equal(until) {
			t.Fatalf("after a same-status rewrite: continuation=%q until=%v, want the transit kept", r.ContinuationState, r.ContinuationUntil)
		}
	})

	t.Run("a save from a stale copy keeps it", func(t *testing.T) {
		capacityStampedRun(t, s, "follow_stale", store.RunStatusFailedResumable, store.RunCredStamp{})
		stale := loadForTransit(t, s, "follow_stale")
		promoteTransit(t, s, "follow_stale", until)
		// The stale copy is a version behind; a saver re-reads before it
		// writes, as every full-document saver does, and its copy still
		// carries no instant.
		fresh := loadForTransit(t, s, "follow_stale")
		fresh.ContinuationState = stale.ContinuationState
		fresh.ContinuationUntil = stale.ContinuationUntil
		fresh.Name = "renamed"
		if err := s.SaveRun(ctx, fresh); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		if r := loadForTransit(t, s, "follow_stale"); !r.InCapacityTransit(time.Now()) || !r.ContinuationUntil.Equal(until) {
			t.Fatalf("after a save carrying no instant: continuation=%q until=%v, want the transit kept", r.ContinuationState, r.ContinuationUntil)
		}
	})

	t.Run("a flip clears it and its revert restores it", func(t *testing.T) {
		flipper := store.AsQueuedFlipper(s)
		if flipper == nil {
			t.Skip("the store cannot flip a run to queued")
		}
		transitRun(t, s, "follow_flip", store.RunCredStamp{}, until)
		flip, ok, err := flipper.FlipToQueued(ctx, "follow_flip", store.RunStatusFailedResumable, time.Now())
		if err != nil || !ok {
			t.Fatalf("flip: %v %v", ok, err)
		}
		if r := loadForTransit(t, s, "follow_flip"); r.ContinuationUntil != nil {
			t.Fatalf("queued run keeps until=%v", r.ContinuationUntil)
		}
		if ok, err := flipper.RevertQueuedFlip(ctx, "follow_flip", flip, ""); err != nil || !ok {
			t.Fatalf("revert: %v %v", ok, err)
		}
		if r := loadForTransit(t, s, "follow_flip"); !r.InCapacityTransit(time.Now()) || !r.ContinuationUntil.Equal(until) {
			t.Fatalf("after the revert: continuation=%q until=%v, want the transit restored", r.ContinuationState, r.ContinuationUntil)
		}
	})
}
