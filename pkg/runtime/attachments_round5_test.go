package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Round 5 (H-10) of the adversarial loop on #2086, the runtime side.

// ctxListStore lists the run's attachments only while its ctx lives — a
// store that honours the ctx on the list call itself.
type ctxListStore struct{ store.RunStore }

func (ctxListStore) Root() string { return "" }

func (c ctxListStore) ListAttachments(ctx context.Context, runID string) ([]store.AttachmentRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.RunStore.ListAttachments(ctx, runID)
}

// The list site is a fetch site too: the run's deadline cutting the list
// carries no store code, and the deadline is said once.
func TestADeadlineCuttingTheListCarriesNoStoreCode(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fs := tmpStore(t)
	attachedRun(t, fs, "run-deadline-list")
	e := engineOn(t, ctxListStore{fs})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := e.materializeAttachments(ctx, "run-deadline-list")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline in the chain", err)
	}
	var rt *RuntimeError
	if errors.As(err, &rt) {
		t.Fatalf("the deadline's list failure carries the store code %s", rt.Code)
	}
	if n := strings.Count(err.Error(), context.DeadlineExceeded.Error()); n != 1 {
		t.Fatalf("the deadline is said %d times: %v", n, err)
	}
}

// A directory the runner could not create is its own failure, not the
// store's: no ATTACHMENTS_UNFETCHABLE, whose remedy blames the store.
func TestALocalStepFailureIsNotTheStores(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	fs := tmpStore(t)
	attachedRun(t, fs, "run-local-step")
	e := engineOn(t, noRootStore{fs})
	_, err := e.materializeAttachments(context.Background(), "run-local-step")
	if err == nil {
		t.Fatal("a materialisation under a missing TMPDIR succeeded")
	}
	var rt *RuntimeError
	if errors.As(err, &rt) {
		t.Fatalf("a local step's failure carries the store code %s: %v", rt.Code, err)
	}
}
