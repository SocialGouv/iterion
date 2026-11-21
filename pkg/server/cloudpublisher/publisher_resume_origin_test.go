package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// claimHookStore is the filesystem store with a one-shot hook run on entry
// of FlipToQueued, before it delegates: a successor recreated inside the
// claim. Every call is forwarded with the caller's context, and the store
// endorses itself as the one it wraps.
type claimHookStore struct {
	*store.FilesystemRunStore
	mu         sync.Mutex
	beforeFlip func() error
	hookErr    error
}

func (s *claimHookStore) CheckBoundObservationStore(actual store.RunStore) error {
	if a, ok := actual.(*claimHookStore); !ok || a != s {
		return fmt.Errorf("%w: %T is not this test double", store.ErrRunDataCapability, actual)
	}
	return nil
}

func (s *claimHookStore) FlipToQueued(ctx context.Context, id string, from store.RunStatus, at time.Time) (store.QueuedFlip, bool, error) {
	s.mu.Lock()
	h := s.beforeFlip
	s.beforeFlip = nil
	s.mu.Unlock()
	if h != nil {
		if err := h(); err != nil {
			s.mu.Lock()
			s.hookErr = errors.Join(s.hookErr, err)
			s.mu.Unlock()
		}
	}
	return s.FilesystemRunStore.FlipToQueued(ctx, id, from, at)
}

// nonEndorsingClaimStore forwards every call but does not endorse itself:
// the method it promotes refuses any receiver but the native store.
type nonEndorsingClaimStore struct{ *store.FilesystemRunStore }

const resumeOriginRun = "run-resume-origin"

// seedResumeOriginRun creates the run under resumeOriginRun, named marker,
// parked failed_resumable — what an operator resume republishes.
func seedResumeOriginRun(ctx context.Context, st *store.FilesystemRunStore, marker string) error {
	if _, err := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: resumeOriginRun, WorkflowName: marker, Format: store.LegacyValueFormat}); err != nil {
		return err
	}
	r, err := st.LoadRun(ctx, resumeOriginRun)
	if err != nil {
		return err
	}
	r.Name = marker
	r.Status = store.RunStatusFailedResumable
	return st.SaveRun(ctx, r)
}

// replaceResumeOriginRun deletes and prunes the run, then seeds its
// successor B in the same status — the only way a new birth takes an ID.
func replaceResumeOriginRun(ctx context.Context, st *store.FilesystemRunStore) error {
	if err := st.DeleteRun(ctx, resumeOriginRun); err != nil {
		return err
	}
	if _, err := st.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	return seedResumeOriginRun(ctx, st, "B")
}

// W8b: SubmitResume republishes the run its checked caller admitted, or
// nothing. (i) A replaced by B before the publisher's load: the document it
// reads is verified, refused typed. (ii) A replaced inside the claim, after
// that check: the bound flip refuses typed. (iii) A store that may drop the
// binding is refused before the claim. Each: nothing published, B (or A)
// not queued. Controls: the admitted run is claimed and published once; an
// unchecked resume (the zero ActionOrigin) claims and publishes B, as every
// caller without an expectation does.
func TestSubmitResumeExpectOrigin(t *testing.T) {
	ctx := context.Background()
	cs := rollbackTestSource()
	wf := &ir.Workflow{Name: "w"}
	for _, tc := range []struct {
		name      string
		checked   bool
		replace   string // "", "before", "claim"
		endorsing bool
		want      error
	}{
		{"i-replaced-before-the-load", true, "before", true, store.ErrRunDataIdentityChanged},
		{"ii-replaced-inside-the-claim", true, "claim", true, store.ErrRunDataIdentityChanged},
		{"iii-a-store-that-drops-the-binding", true, "", false, store.ErrRunDataCapability},
		{"control-the-admitted-run", true, "", true, nil},
		{"control-unchecked-claims-the-successor", false, "claim", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := seedResumeOriginRun(ctx, native, "A"); err != nil {
				t.Fatal(err)
			}
			obsA, err := runview.CaptureRunOriginIn(ctx, native, resumeOriginRun)
			if err != nil {
				t.Fatal(err)
			}
			hook := &claimHookStore{FilesystemRunStore: native}
			var st store.RunStore = hook
			if !tc.endorsing {
				st = nonEndorsingClaimStore{native}
			}
			switch tc.replace {
			case "before":
				if err := replaceResumeOriginRun(ctx, native); err != nil {
					t.Fatal(err)
				}
			case "claim":
				hook.beforeFlip = func() error { return replaceResumeOriginRun(ctx, native) }
			}
			var published []string
			p := &Publisher{store: st, logger: iterlog.New(iterlog.LevelError, io.Discard), publishRun: func(_ context.Context, m *queue.RunMessage) error {
				published = append(published, m.RunID)
				return nil
			}}
			spec := runview.ResumeSpec{RunID: resumeOriginRun, FilePath: "main.bot", Source: cs.Files["main.bot"]}
			if tc.checked {
				spec.ExpectOrigin = runview.ExpectRunOrigin(obsA)
			}
			err = p.SubmitResume(ctx, spec, wf, cs)
			if hook.hookErr != nil {
				t.Fatalf("the replacement failed: %v", hook.hookErr)
			}
			got, lerr := native.LoadRun(ctx, resumeOriginRun)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if tc.replace != "" && got.BirthID == obsA.Header().BirthID {
				t.Fatal("fixture: the replacement did not run")
			}
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("SubmitResume = %v, want %v", err, tc.want)
				}
				if len(published) != 0 || got.Status != store.RunStatusFailedResumable {
					t.Fatalf("a refused resume published %v and left the run %s, want nothing published and the run failed_resumable", published, got.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("SubmitResume = %v, want the resume published", err)
			}
			if len(published) != 1 || got.Status != store.RunStatusQueued {
				t.Fatalf("published %v, run %s (birth %q), want one publication and the run queued", published, got.Status, got.BirthID)
			}
		})
	}
}
