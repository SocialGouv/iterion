package runview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

type spawnAdmissionStore struct {
	store.RunStore
	fs      *store.FilesystemRunStore
	svc     *Service
	kind    string
	once    sync.Once
	reached bool
	err     error
}

func (s *spawnAdmissionStore) Unwrap() store.RunStore { return s.RunStore }
func (s *spawnAdmissionStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	var readErr error
	if s.svc != nil && id == "run" && s.svc.manager.Active(id) {
		s.once.Do(func() {
			s.reached = true
			switch s.kind {
			case "normal":
				return
			case "infra":
				readErr = errors.New("transient spawn read failure")
				return
			case "future":
				path := filepath.Join(s.fs.Root(), "runs", id, "run.json")
				var raw []byte
				if raw, s.err = os.ReadFile(path); s.err != nil {
					return
				}
				var doc map[string]json.RawMessage
				if s.err = json.Unmarshal(raw, &doc); s.err != nil {
					return
				}
				doc["format_version"] = json.RawMessage("99")
				if raw, s.err = json.Marshal(doc); s.err != nil {
					return
				}
				s.err = os.WriteFile(path, raw, 0600)
				return
			}
			if s.err = s.fs.DeleteRun(context.Background(), id); s.err != nil {
				return
			}
			if _, s.err = s.fs.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); s.err != nil {
				return
			}
			_, s.err = store.CreateRunWithValueFormat(context.Background(), s.fs, store.RunCreateSpec{ID: id, Format: store.ExactValueFormat})
		})
	}
	if s.err != nil {
		return nil, s.err
	}
	if readErr != nil {
		return nil, readErr
	}
	return s.RunStore.LoadRun(ctx, id)
}
func TestServiceRejectsChangedFormatBeforeAuxiliaries(t *testing.T) {
	// swapStartDeclared is a package test seam, intentionally nonparallel.
	for _, kind := range []string{"normal", "infra", "replaced", "future"} {
		t.Run(kind, func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			spy := &spawnAdmissionStore{RunStore: fs, fs: fs, kind: kind}
			svc, err := NewService("", WithStore(spy), WithLogger(iterlog.Nop()))
			if err != nil {
				t.Fatal(err)
			}
			spy.svc = svc
			t.Cleanup(func() { stopService(t, svc) })
			starts := 0
			restore := swapStartDeclared(func(context.Context, supervise.Observer, supervise.Injector, string, []supervise.Spec, *iterlog.Logger) func() {
				starts++
				return func() {}
			})
			defer restore()
			source := `schema checked:
  ok: bool
prompt watch_policy:
  """
  Observe only.
  """
supervisor watchdog:
  watches: [check]
  model: "anthropic/fixture"
  system: watch_policy
  cooldown: "30s"
compute check:
  output: checked
  expr:
    ok: "true"
workflow probe:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`
			var outcome error
			res, err := svc.Launch(t.Context(), LaunchSpec{RunID: "run", Source: source, WorkDir: t.TempDir(), Supervisors: "on", OnOutcome: func(e error) { outcome = e }})
			if err != nil {
				t.Fatal(err)
			}
			awaitRunCompletion(t, res.Done, "spawn admission did not finish")
			if spy.err != nil || !spy.reached {
				t.Fatalf("replacement reached=%v error=%v", spy.reached, spy.err)
			}
			switch kind {
			case "replaced", "future":
				want := store.ErrValueFormatChanged
				if kind == "future" {
					want = store.ErrUnsupportedValueFormat
				}
				if !errors.Is(outcome, want) {
					t.Fatalf("lost refusal: %v want %v", outcome, want)
				}
				if starts != 0 {
					t.Fatalf("supervisor started after format refusal: %d", starts)
				}
			default:
				if outcome != nil || starts != 1 {
					t.Fatalf("control outcome=%v starts=%d", outcome, starts)
				}
			}
		})
	}
}
