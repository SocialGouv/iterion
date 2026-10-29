package cloudpublisher

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The schedule fence's promise is about the REAL path: the publisher's
// SubmitLaunch persisting through the Mongo run store, inside the transaction
// cloudsched.MongoStore.WithSchedule installs. The earlier proofs each covered
// a proxy of it — a file run store behind the real fence, an ad-hoc InsertOne
// behind the real transaction, a guard that refuses before any write — so a
// mutation taking the session out of the real SaveRun, or a takeover landing
// after a commit, had no witness. This suite assembles the real publisher and
// the real run store on one Mongo client, behind the real fence, and checks
// the barrier in both orders: a committed run is published and survives a
// later takeover, a refused persistence publishes nothing and leaves no row.
//
// Gated like every other Mongo suite (ITERION_TEST_MONGO_URI, replica set —
// the path under test IS a multi-document transaction); the mongo-conformance
// CI job runs it.
func TestScheduleFenceMongoPublisherRunStoreTransaction(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set — skipping the publisher-to-run-store transaction suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_schedule_fence_pub_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
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

	// published records the runs the queue publish fired for, AND the status
	// the row already carried at that instant: persistence must be visible
	// before publication, never the other way around.
	type publication struct {
		runID  string
		status store.RunStatus
	}
	var publications []publication
	p := &Publisher{
		store:  rst,
		logger: iterlog.New(iterlog.LevelError, io.Discard),
		publishRun: func(pctx context.Context, m *queue.RunMessage) error {
			r, err := rst.LoadRun(pctx, m.RunID)
			if err != nil {
				return err
			}
			publications = append(publications, publication{runID: m.RunID, status: r.Status})
			return nil
		},
	}
	launch := func(fctx context.Context, runID string) error {
		_, err := p.SubmitLaunch(fctx, runID, spec, wf, &runview.CompiledSource{Hash: "hash"})
		return err
	}
	// takeOver simulates the successor replica the fence exists for: the lease
	// row changes owner while the paused holder's context still believes in its
	// admission. The renewer cannot undo it — Renew matches on the owner.
	takeOver := func(schedule string) {
		t.Helper()
		if _, err := db.Collection(lease.Collection).UpdateOne(ctx,
			bson.M{"_id": "schedule-launch:" + schedule},
			bson.M{"$set": bson.M{"owner": "successor"}}); err != nil {
			t.Fatal(err)
		}
	}
	loadStatus := func(runID string) (store.RunStatus, error) {
		r, err := rst.LoadRun(base, runID)
		if err != nil {
			return "", err
		}
		return r.Status, nil
	}
	launchCommits := func(schedule string) int64 {
		t.Helper()
		var doc struct {
			LaunchCommits int64 `bson:"launch_commits"`
		}
		if err := db.Collection(lease.Collection).FindOne(ctx, bson.M{"_id": "schedule-launch:" + schedule}).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		return doc.LaunchCommits
	}

	// 1. A fenced launch commits the receipt and the run in one transaction,
	//    then publishes: row queued, publish fired, receipt counted. The
	//    receipt is read INSIDE the fence: its row is released when the term
	//    ends.
	ok, err := sched.WithSchedule(base, "fence-commit", func(fctx context.Context) {
		if err := launch(fctx, "run-fence-commit"); err != nil {
			t.Errorf("fenced launch: %v", err)
			return
		}
		if got := launchCommits("fence-commit"); got != 1 {
			t.Errorf("launch_commits = %d, want 1: the receipt and the run commit together", got)
		}
	})
	if err != nil || !ok {
		t.Fatalf("WithSchedule: %v %v", ok, err)
	}
	if status, err := loadStatus("run-fence-commit"); err != nil || status != store.RunStatusQueued {
		t.Fatalf("committed run: status=%q err=%v, want a queued row", status, err)
	}
	if len(publications) != 1 || publications[0].runID != "run-fence-commit" || publications[0].status != store.RunStatusQueued {
		t.Fatalf("publications = %+v, want the committed run published AFTER its row reads queued", publications)
	}

	// 2. Takeover BEFORE the persist: the fenced launch is refused, no row
	//    exists, nothing was published.
	ok, err = sched.WithSchedule(base, "fence-takeover-first", func(fctx context.Context) {
		takeOver("fence-takeover-first")
		if err := launch(fctx, "run-fence-takeover-first"); !errors.Is(err, schedgate.ErrMaintenanceHold) {
			t.Errorf("launch behind a stolen lease: %v, want ErrMaintenanceHold", err)
		}
	})
	if err != nil || !ok {
		t.Fatalf("WithSchedule: %v %v", ok, err)
	}
	if _, err := loadStatus("run-fence-takeover-first"); err == nil {
		t.Fatal("a takeover-refused launch left a run row behind")
	}
	if len(publications) != 1 {
		t.Fatalf("publications = %+v, want no publish for the refused launch", publications)
	}

	// 3. Commit THEN takeover: the committed run survives and the paused
	//    holder's NEXT persist — behind the successor's takeover — is refused.
	ok, err = sched.WithSchedule(base, "fence-commit-then-takeover", func(fctx context.Context) {
		if err := launch(fctx, "run-fence-kept"); err != nil {
			t.Errorf("fenced launch: %v", err)
			return
		}
		takeOver("fence-commit-then-takeover")
		if err := launch(fctx, "run-fence-refused"); !errors.Is(err, schedgate.ErrMaintenanceHold) {
			t.Errorf("launch after the takeover: %v, want ErrMaintenanceHold", err)
		}
	})
	if err != nil || !ok {
		t.Fatalf("WithSchedule: %v %v", ok, err)
	}
	if status, err := loadStatus("run-fence-kept"); err != nil || status != store.RunStatusQueued {
		t.Fatalf("the committed run must survive the takeover: status=%q err=%v", status, err)
	}
	if _, err := loadStatus("run-fence-refused"); err == nil {
		t.Fatal("the post-takeover persist left a run row behind")
	}
	if len(publications) != 2 || publications[1].runID != "run-fence-kept" {
		t.Fatalf("publications = %+v, want exactly the committed run published", publications)
	}

	// 4. The session witness: drive the REAL Mongo SaveRun through the fence's
	//    persistence guard and fail AFTER the write, so the transaction aborts.
	//    A mutation taking the session out of SaveRun's write would leave the
	//    row committed — this is the check the ad-hoc InsertOne never gave.
	injected := errors.New("injected post-write failure")
	ok, err = sched.WithSchedule(base, "fence-abort", func(fctx context.Context) {
		err := store.PersistLaunch(fctx, func(tx context.Context) error {
			if mongo.SessionFromContext(tx) == nil {
				t.Error("the real run write carries no transaction")
			}
			if err := rst.SaveRun(tx, &store.Run{ID: "run-fence-aborted", BotID: "bot", Status: store.RunStatusQueued}); err != nil {
				return err
			}
			return injected
		})
		if !errors.Is(err, schedgate.ErrMaintenanceHold) {
			t.Errorf("aborted persist: %v, want ErrMaintenanceHold", err)
		}
		// Read the receipt INSIDE the fence: its row is released with the term.
		var leaseDoc bson.M
		if err := db.Collection(lease.Collection).FindOne(ctx, bson.M{"_id": "schedule-launch:fence-abort"}).Decode(&leaseDoc); err != nil {
			t.Error(err)
			return
		}
		if _, exists := leaseDoc["launch_commits"]; exists {
			t.Errorf("the receipt survived the aborted run write: %v", leaseDoc)
		}
	})
	if err != nil || !ok {
		t.Fatalf("WithSchedule: %v %v", ok, err)
	}
	if _, err := loadStatus("run-fence-aborted"); err == nil {
		t.Fatal("the aborted transaction left the run committed — the real SaveRun escaped the fence's session")
	}
}
