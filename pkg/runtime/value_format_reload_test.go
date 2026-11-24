package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type invocationThirdReadStore struct {
	store.RunStore
	swap     func() error
	loads    int
	switched bool
}

func (s *invocationThirdReadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.loads++
	if s.loads == 3 {
		if err := s.swap(); err != nil {
			return nil, err
		}
		s.switched = true
	}
	return s.RunStore.LoadRun(ctx, id)
}

type invocationVarsSpy struct {
	NodeExecutor
	after       *bool
	postRefusal int
}

func (s *invocationVarsSpy) SetVars(map[string]any) {
	if *s.after {
		s.postRefusal++
	}
}
func TestPublicCacheReloadRefusalStopsCallbacks(t *testing.T) {
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-format-control", true: "replace-before-cache-read"}[swap], func(t *testing.T) {
			st := tmpStore(t)
			r, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "cache-reload", Format: 1})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			wrapper := &invocationThirdReadStore{RunStore: st}
			wrapper.swap = func() error {
				if !swap {
					return nil
				}
				if err := st.DeleteRun(ctx, r.ID); err != nil {
					return err
				}
				if _, err := st.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
					return err
				}
				_, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: r.ID, Format: 2})
				return err
			}
			calls := 0
			e := admissionSnapshotEngine(t, wrapper, nil, &calls)
			spy := &invocationVarsSpy{NodeExecutor: e.executor, after: &wrapper.switched}
			e.executor = spy
			err = e.Run(ctx, r.ID, nil)
			t.Logf("swap=%v loads=%d vars_after_read=%d node_calls=%d error=%v", swap, wrapper.loads, spy.postRefusal, calls, err)
			if !wrapper.switched {
				t.Fatal("fixture missed authoritative cache read")
			}
			if swap {
				if !errors.Is(err, store.ErrValueFormatChanged) || spy.postRefusal != 0 || calls != 0 {
					t.Fatalf("CACHE_REFUSAL_CONTINUED: error=%v callbacks=%d node_calls=%d", err, spy.postRefusal, calls)
				}
			} else if err != nil || spy.postRefusal == 0 || calls != 1 {
				t.Fatalf("HEALTHY_CACHE_REFUSED: error=%v callbacks=%d node_calls=%d", err, spy.postRefusal, calls)
			}
		})
	}
}

func TestCacheOrdinaryReadFailureRemainsBestEffort(t *testing.T) {
	st := tmpStore(t)
	r, err := st.CreateRun(t.Context(), "cache-io", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.BindRunValueFormat(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &invocationThirdReadStore{RunStore: st}
	wrapper.swap = func() error { wrapper.switched = true; return errors.New("ordinary optional metadata read I/O") }
	calls := 0
	e := admissionSnapshotEngine(t, wrapper, nil, &calls)
	spy := &invocationVarsSpy{NodeExecutor: e.executor, after: &wrapper.switched}
	e.executor = spy
	err = e.Run(ctx, r.ID, nil)
	if err != nil || !wrapper.switched || spy.postRefusal != 1 || calls != 1 {
		t.Fatalf("ORDINARY_CACHE_IO_CHANGED: error=%v switched=%v callbacks=%d node=%d", err, wrapper.switched, spy.postRefusal, calls)
	}
}

func TestResumeParkPreservesKnownFormatRefusal(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		for _, swap := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancel=%v/swap=%v", cancelled, swap), func(t *testing.T) {
				st := tmpStore(t)
				r, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "park", Format: 1})
				if err != nil {
					t.Fatal(err)
				}
				ctx, err := store.BindRunValueFormat(t.Context(), r)
				if err != nil {
					t.Fatal(err)
				}
				if swap {
					if err = st.DeleteRun(ctx, r.ID); err != nil {
						t.Fatal(err)
					}
					if _, err = st.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: r.ID, Format: 2}); err != nil {
						t.Fatal(err)
					}
				}
				cause := errors.New("ordinary sandbox startup failure")
				if cancelled {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					cause = context.Canceled
				}
				calls := 0
				spy := &setupRefusalStore{RunStore: st}
				e := admissionSnapshotEngine(t, spy, nil, &calls)
				got := e.parkResumeSandboxFailure(ctx, r.ID, &store.Checkpoint{NodeID: "probe"}, "probe", cause)
				fresh, readErr := st.LoadRun(t.Context(), r.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("cancel=%v swap=%v returned=%v status=%s writer_calls=%d events=%d", cancelled, swap, got, fresh.Status, spy.writes, spy.events)
				if swap {
					if !errors.Is(got, store.ErrValueFormatChanged) || fresh.Status != store.RunStatusRunning || spy.events != 0 || spy.writes != 0 {
						t.Fatalf("PARK_LOST_KNOWN_REFUSAL: returned=%v status=%s events=%d", got, fresh.Status, spy.events)
					}
				} else if cancelled {
					if !errors.Is(got, ErrRunCancelled) || fresh.Status != store.RunStatusCancelled {
						t.Fatalf("NORMAL_CANCEL_CHANGED: %v %s", got, fresh.Status)
					}
				} else if !errors.Is(got, cause) || fresh.Status != store.RunStatusFailedResumable {
					t.Fatalf("NORMAL_PARK_CHANGED: %v %s", got, fresh.Status)
				}
			})
		}
	}
}

type invocationFinalizeReloadStore struct {
	store.RunStore
	loads   int
	replace func() error
}

func (s *invocationFinalizeReloadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.loads++
	if s.loads == 2 {
		if err := s.replace(); err != nil {
			return nil, err
		}
	}
	return s.RunStore.LoadRun(ctx, id)
}
func TestFinalMetadataRefusalStopsCleanup(t *testing.T) {
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "replaced-before-final-metadata"}[swap], func(t *testing.T) {
			repo, tip := initBareishRepo(t)
			wt := filepath.Join(t.TempDir(), "wt")
			gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
			t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })
			addCommit(t, wt, "result.txt", "result\n", "produced output")
			st := tmpStore(t)
			r, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "final-reload", Format: 1})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			wrapper := &invocationFinalizeReloadStore{RunStore: st}
			wrapper.replace = func() error {
				if !swap {
					return nil
				}
				if err := st.DeleteRun(ctx, r.ID); err != nil {
					return err
				}
				if _, err := st.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
					return err
				}
				fresh, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: r.ID, Format: 2})
				if err != nil {
					return err
				}
				fresh.WorkDir = wt
				fresh.Worktree = true
				fresh.RepoRoot = repo
				return st.SaveRun(t.Context(), fresh)
			}
			calls := 0
			e := admissionSnapshotEngine(t, wrapper, nil, &calls)
			e.workDir = wt
			e.mergeInto = "none"
			e.runName = "final-reload"
			cleanupCalls := 0
			e.finalizeOnExit(ctx, r.ID, &worktreeContext{repoRoot: repo, wtPath: wt, originalBranch: "main", originalTip: tip}, func() { cleanupCalls++; gittest.Run(t, repo, "worktree", "remove", "--force", wt) }, nil)
			_, statErr := os.Stat(wt)
			t.Logf("swap=%v loads=%d cleanup=%d workspace_stat=%v", swap, wrapper.loads, cleanupCalls, statErr)
			if wrapper.loads != 2 {
				t.Fatalf("fixture missed final metadata reload: %d", wrapper.loads)
			}
			if swap {
				if cleanupCalls != 0 || statErr != nil {
					t.Fatalf("FINAL_REFUSAL_CLEANED_SUCCESSOR_WORKSPACE: callbacks=%d stat=%v", cleanupCalls, statErr)
				}
			} else if cleanupCalls != 1 || !os.IsNotExist(statErr) {
				t.Fatalf("NORMAL_FINALIZE_CHANGED: callbacks=%d stat=%v", cleanupCalls, statErr)
			}
		})
	}
}

func TestCacheFutureHeaderStopsCallbacks(t *testing.T) {
	st := tmpStore(t)
	r, err := st.CreateRun(t.Context(), "cache-future", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.BindRunValueFormat(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &invocationThirdReadStore{RunStore: st}
	wrapper.swap = func() error {
		path := filepath.Join(st.Root(), "runs", r.ID, "run.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc map[string]json.RawMessage
		if err = json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		doc["format_version"] = json.RawMessage("99")
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
		return os.WriteFile(path, raw, 0600)
	}
	calls := 0
	e := admissionSnapshotEngine(t, wrapper, nil, &calls)
	spy := &invocationVarsSpy{NodeExecutor: e.executor, after: &wrapper.switched}
	e.executor = spy
	err = e.Run(ctx, r.ID, nil)
	if !wrapper.switched || !errors.Is(err, store.ErrUnsupportedValueFormat) || spy.postRefusal != 0 || calls != 0 {
		t.Fatalf("FUTURE_CACHE_CALLBACK: error=%v switched=%v callbacks=%d node=%d", err, wrapper.switched, spy.postRefusal, calls)
	}
}
