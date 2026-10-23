package cloudsched

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Different slots, same schedule: a per-intention claim does not protect the
// interval between the overlap read and persistence of the admitted run.
func TestRound5ReplicasSerializeOverlapThroughLaunch(t *testing.T) {
	assertReplicasSerializeOverlap(t, NewMemoryStore())
}

func TestRound5MongoReplicasSerializeOverlapThroughLaunch(t *testing.T) {
	assertReplicasSerializeOverlap(t, NewMongoStore(scheduleMongo(t)))
}

func assertReplicasSerializeOverlap(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	runs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	sb := ScheduledBot{ID: "held", BotID: "bot", Cron: "* * * * *", NextFireAt: now, Overlap: "skip"}
	if err := st.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	var hold atomic.Bool
	hold.Store(true)
	var launches atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	tick := func(at time.Time) *Ticker {
		return &Ticker{Store: st, Now: func() time.Time { return at },
			Gate: func(ctx context.Context, sb ScheduledBot) (bool, string, schedgate.TickRecord) {
				out := schedgate.Apply(ctx, schedgate.GateInput{Policy: sb.Policy(), Lister: runs, ScheduleID: sb.ID, Now: at})
				return out.Proceed, out.GuardStdout, out.Record
			},
			Launch: func(ctx context.Context, sb ScheduledBot) error {
				if hold.Load() {
					return schedgate.ErrMaintenanceHold
				}
				select {
				case <-entered:
				default:
					close(entered)
					<-release
				}
				r, err := runs.CreateRun(ctx, fmt.Sprintf("run-%d", launches.Load()+1), sb.BotID, nil)
				if err != nil {
					return err
				}
				r.Status, r.Source = store.RunStatusRunning, &store.RunSource{Kind: store.RunSourceKindSchedule, ScheduleID: sb.ID}
				if err := runs.SaveRun(ctx, r); err != nil {
					return err
				}
				launches.Add(1)
				return nil
			},
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := tick(now.Add(time.Duration(i) * time.Minute)).Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	hold.Store(false)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = tick(now.Add(140 * time.Second)).Tick(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first replica did not reach launch")
	}
	// Another due slot arrives while the first run is not persisted yet.
	_, err = tick(now.Add(3 * time.Minute)).Tick(ctx)
	before := launches.Load()
	close(release)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if before != 0 || launches.Load() != 1 {
		t.Fatalf("overlap read raced launch persistence: before release=%d total=%d, want 0/1", before, launches.Load())
	}
}
