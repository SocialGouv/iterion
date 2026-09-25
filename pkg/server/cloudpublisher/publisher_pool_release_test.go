package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

type failingPoolBundleStore struct {
	secrets.RunSecretsStore
	beforeFailure func()
	err           error
}

func (s failingPoolBundleStore) Put(context.Context, secrets.RunSecretsRecord) error {
	s.beforeFailure()
	return s.err
}

// Credential resolution may fail after Acquire, even before the publisher
// arms its deferred cleanup. That cleanup must release its own grant, never
// whichever lease a newer attempt has acquired in the meantime.
func TestPublisher_PoolCleanupOwnsItsGrant(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, successor := range []bool{false, true} {
			name := "launch"
			if resume {
				name = "resume"
			}
			if successor {
				name += "/successor"
			} else {
				name += "/sole_acquisition"
			}
			t.Run(name, func(t *testing.T) {
				f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5, MaxRunsPerDay: 2})
				st, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				f.pub.store = st
				ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
				// This publisher has no identity store; admit the team explicitly.
				if err := f.pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true,
					Audience: credpool.Audience{Teams: []string{poolTeam}}}); err != nil {
					t.Fatal(err)
				}
				const runID = "run-cleanup"
				if resume {
					seedDonorRun(t, st, runID, nil, nil)
				}
				failure := errors.New("bundle store unavailable")
				var newer credpool.Lease
				var acquired bool
				f.pub.runSecrets = failingPoolBundleStore{RunSecretsStore: f.rs, err: failure, beforeFailure: func() {
					if _, err := f.leases.GetOpenByRun(ctx, runID); err != nil {
						t.Fatalf("premise: no lease acquired before bundle failure: %v", err)
					}
					acquired = true
					if !successor {
						return
					}
					_, err := f.pub.credPool.Acquire(ctx, credpool.Request{
						RunID: runID, OrgID: poolOrg, TenantID: poolTeam, UserID: "requester",
						Wants: []credpool.Credential{{Source: credpool.SourceOAuth, Ref: "claude_code"}},
					})
					if err != nil {
						t.Fatalf("successor Acquire: %v", err)
					}
					newer, err = f.leases.GetOpenByRun(ctx, runID)
					if err != nil {
						t.Fatal(err)
					}
				}}
				wf := &ir.Workflow{Name: "wf"}
				cs := &runview.CompiledSource{Hash: "hash"}
				if resume {
					err = f.pub.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, wf, cs)
				} else {
					_, err = f.pub.SubmitLaunch(ctx, runID, runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, wf, cs)
				}
				if !errors.Is(err, failure) || !acquired {
					t.Fatalf("want failure after acquisition, got acquired=%v, err=%v", acquired, err)
				}
				open, err := f.leases.ListOpenByRun(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				wantRuns := 0
				if successor {
					// Superseding an unreported attempt does not refund it;
					// the successor consumes a second unit. Late cleanup must
					// not return that successor's unit either.
					wantRuns = 2
					if len(open) != 1 || open[0].ID != newer.ID {
						t.Fatalf("cleanup closed the successor: open=%v, want lease %s", open, newer.ID)
					}
				} else if len(open) != 0 {
					t.Fatalf("failed acquisition retained its lease: %v", open)
				}
				pledgeID := credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")
				day, _, err := f.ledger.Usage(ctx, pledgeID, time.Now().UTC())
				if err != nil || day.Runs != wantRuns {
					t.Fatalf("daily runs=%d, want %d (%v)", day.Runs, wantRuns, err)
				}
			})
		}
	}
}
