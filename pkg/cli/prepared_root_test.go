package cli

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
)

type d1BeforeEngineWriter struct {
	once bool
	call func()
}

func (w *d1BeforeEngineWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		w.call()
	}
	return len(p), nil
}
func TestPreparedCLIRootCancellationBeforeEngine(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, mode := range []string{"normal", "cancel", "peer", "existing"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "launch.bot")
			if err := os.WriteFile(path, []byte(contextLaunchSource), 0600); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "existing" {
				if _, err = st.CreateRun(t.Context(), "run", "probe", nil); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var seamErr error
			var birthSeen bool
			var peer store.RunExecutionAuthority
			printer := &Printer{Format: OutputHuman, W: &d1BeforeEngineWriter{call: func() {
				r, e := st.LoadRun(t.Context(), "run")
				if e != nil {
					seamErr = e
					return
				}
				birthSeen = true
				if mode == "peer" {
					var ok bool
					peer, ok, e = st.AcquireRunExecutionIf(t.Context(), "run", store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}, store.ExecutionStart)
					if e != nil {
						seamErr = e
						return
					}
					if !ok {
						seamErr = errors.New("peer not acquired")
						return
					}
				}
				if mode != "normal" {
					cancel()
				}
			}}}
			got := RunRun(ctx, RunOptions{File: path, RunID: "run", Background: mode == "existing", StoreDir: st.Root(), Executor: &contextLaunchCloser{}, NoInteractive: true, Sandbox: "none"}, printer)
			if seamErr != nil || !birthSeen {
				t.Fatalf("pre-engine printer missed birth: %v %v", birthSeen, seamErr)
			}
			r, err := st.LoadRun(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("mode=%s err=%v status=%s", mode, got, r.Status)
			switch mode {
			case "normal":
				if got != nil || r.Status != store.RunStatusFinished {
					t.Fatal(got, r.Status)
				}
			case "cancel":
				if !errors.Is(got, runtime.ErrRunCancelled) || r.Status != store.RunStatusCancelled {
					t.Fatalf("CLI_BIRTH_NOT_WITHDRAWN: %v %s", got, r.Status)
				}
			case "peer":
				if !errors.Is(got, runtime.ErrAdmissionStopped) || !peer.Matches(r) || r.Status != store.RunStatusRunning {
					t.Fatalf("CLI_PEER_WITHDRAWN: %v %s", got, r.Status)
				}
			case "existing":
				if !errors.Is(got, context.Canceled) || r.Status != store.RunStatusRunning {
					t.Fatalf("EXISTING_WITHDRAWN: %v %s", got, r.Status)
				}
			}
		})
	}
}
