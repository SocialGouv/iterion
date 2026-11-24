package runner

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"testing"
	"time"
)

type unpreparedInvocationStore struct {
	*store.FilesystemRunStore
	mode string
}

func (s unpreparedInvocationStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.mode == "unreadable" {
		return nil, errors.New("read unavailable")
	}
	r, e := s.FilesystemRunStore.LoadRun(ctx, id)
	if r != nil {
		r.ID = "foreign"
	}
	return r, e
}
func TestUnpreparedInvocationDoesNotWrite(t *testing.T) {
	for _, mode := range []string{"foreign", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if _, e = st.CreateRun(ctx, "target", "wf", nil); e != nil {
				t.Fatal(e)
			}
			if e = st.UpdateRunStatus(ctx, "target", store.RunStatusQueued, ""); e != nil {
				t.Fatal(e)
			}
			before, e := st.LoadRun(ctx, "target")
			if e != nil {
				t.Fatal(e)
			}
			r := &Runner{cfg: Config{Store: unpreparedInvocationStore{st, mode}, Logger: iterlog.Nop()}}
			e = r.executeRun(ctx, &queue.RunMessage{RunID: "target", PublishedAtRFC: time.Now().Add(time.Second).Format(time.RFC3339Nano)}, nil, nil)
			if e == nil {
				t.Fatal("missing preparation refusal")
			}
			after, e2 := st.LoadRun(ctx, "target")
			if e2 != nil {
				t.Fatal(e2)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("failed preparation wrote row: before refusal=%v after refusal=%v error=%v", before.PreEngineRefusal, after.PreEngineRefusal, e)
			}
		})
	}
}
