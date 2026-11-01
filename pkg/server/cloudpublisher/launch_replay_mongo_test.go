package cloudpublisher

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// transientOnce is the transient condition the driver's retry loop exists
// for, as a run store decorator: the real SaveRun runs, then the FIRST
// attempt only reports an error carrying the TransientTransactionError
// label. mongo.Session.WithTransaction reads that label off the callback's
// error, aborts the transaction — rolling the write back — and replays the
// callback. Everything downstream of the label is the production path: the
// real driver loop, the real abort, the real mongostore.SaveRun.
type transientOnce struct {
	store.RunStore
	fired bool
	// handed records the CAS version each attempt was given, which is what
	// decides the save's upsert mode: a replay handed an already-advanced
	// version writes with the upsert off against a rolled-back document.
	handed []int64
}

func (s *transientOnce) SaveRun(ctx context.Context, r *store.Run) error {
	s.handed = append(s.handed, r.CASVersion)
	if err := s.RunStore.SaveRun(ctx, r); err != nil {
		return err
	}
	if !s.fired {
		s.fired = true
		return transientTxErr{}
	}
	return nil
}

type transientTxErr struct{}

func (transientTxErr) Error() string { return "injected transient transaction error" }

func (transientTxErr) HasErrorLabel(label string) bool {
	return label == "TransientTransactionError"
}

// A fenced launch whose transaction is replayed still persists its run.
//
// The schedule fence commits the run inside a Mongo transaction, and the
// driver replays that transaction's callback on a TransientTransactionError —
// the callback must therefore be idempotent on its inputs. SaveRun advances
// the run value's CAS version in place, so a callback holding one run value
// offers version 1 on the second attempt while the first attempt's insert has
// been rolled back: the save switches off its upsert, matches nothing, and
// refuses the launch with ErrRunConflict.
//
// What the decorator below injects is the state only one condition produces:
// an abort AFTER the run write succeeded. In this fence the run write is the
// callback's last operation, so in production that abort is a commit-time
// label — a primary stepdown between the write and the commit, a transaction
// killed by transactionLifetimeLimitSeconds — while a write conflict aborts
// before the advance and replays harmlessly. Both of the driver's replay
// routes re-run the same callback (a labeled callback error, session.go:181;
// a commit-time label, session.go:226), and the decorator takes the first.
//
// The whole production chain is real here — publisher SubmitLaunch,
// cloudsched's transaction, mongostore.SaveRun — so only the transient label
// is supplied.
func TestFencedLaunchSurvivesATransactionReplay(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set — skipping the launch transaction replay suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_launch_replay_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
	rst, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: dbName, Blob: nopBlobClient{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = rst.DB().Drop(dctx)
		_ = rst.Close(dctx)
	})
	db := rst.DB()
	if err := cloudsched.EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	sched := cloudsched.NewMongoStore(db)
	base := store.WithIdentity(ctx, "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}

	flaky := &transientOnce{RunStore: rst}
	var published []string
	p := &Publisher{
		store:  flaky,
		logger: iterlog.New(iterlog.LevelError, io.Discard),
	}
	p.publishRun = func(pctx context.Context, m *queue.RunMessage) error {
		// Read the row the publish is about: persistence must be visible
		// before publication, replay or not.
		r, err := flaky.LoadRun(pctx, m.RunID)
		if err != nil {
			return err
		}
		if r.Status != store.RunStatusQueued {
			t.Errorf("published run %s reads status %q, want queued", m.RunID, r.Status)
		}
		published = append(published, m.RunID)
		return nil
	}

	var launchErr error
	// The receipt is read INSIDE the fence: the lease row is released when
	// the term ends. It is the fence's own write in the same transaction, so
	// a rolled-back attempt must leave no trace of its increment.
	var receipts int64
	ok, err := sched.WithSchedule(base, "replay", func(fctx context.Context) {
		_, launchErr = p.SubmitLaunch(fctx, "run-replay", spec, wf, &runview.CompiledSource{Hash: "hash"})
		var leaseDoc struct {
			LaunchCommits int64 `bson:"launch_commits"`
		}
		if err := db.Collection(lease.Collection).FindOne(ctx, bson.M{"_id": "schedule-launch:replay"}).Decode(&leaseDoc); err != nil {
			t.Errorf("reading the fence receipt: %v", err)
			return
		}
		receipts = leaseDoc.LaunchCommits
	})
	if err != nil || !ok {
		t.Fatalf("WithSchedule: ok=%v err=%v", ok, err)
	}
	if launchErr != nil {
		t.Errorf("a replayed launch transaction must still persist its run: %v (CAS versions handed to the attempts: %v)", launchErr, flaky.handed)
	}
	if len(flaky.handed) != 2 {
		t.Fatalf("CAS versions handed to the attempts = %v, want two attempts (the injected transient error must replay the callback)", flaky.handed)
	}
	if flaky.handed[1] != 0 {
		t.Errorf("the replayed attempt was handed CAS version %d, want 0: every attempt saves the run as it stood before the first one", flaky.handed[1])
	}

	r, err := rst.LoadRun(base, "run-replay")
	if err != nil {
		t.Fatalf("the replayed launch committed no run row: %v", err)
	}
	if r.Status != store.RunStatusQueued {
		t.Errorf("committed run status = %q, want queued", r.Status)
	}
	if r.CASVersion != 1 {
		t.Errorf("committed run CAS version = %d, want 1: exactly one write survived the replay", r.CASVersion)
	}
	if len(published) != 1 || published[0] != "run-replay" {
		t.Errorf("published = %v, want the committed run published exactly once", published)
	}
	if receipts != 1 {
		t.Errorf("launch_commits = %d, want 1: only the committed attempt counts a receipt", receipts)
	}
}
