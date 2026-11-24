package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPreparedChildSetupRetirement(t *testing.T) {
	for _, mode := range []string{"ordinary", "ordinary-cancelled", "existing", "forged", "takeover", "metadata", "format-refusal"} {
		t.Run(mode, func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := &preparedCASProbe{FilesystemRunStore: fs}
			p, err := store.PrepareRunInvocation(t.Context(), s, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "child", Status: store.RunStatusQueued}})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("child executor setup refused")
			wantErr := cause
			wantStatus := store.RunStatusFailed
			switch mode {
			case "ordinary-cancelled":
				var cancel context.CancelFunc
				p.Context, cancel = context.WithCancel(p.Context)
				cancel()
			case "existing":
				p, err = store.PrepareRunInvocation(p.Context, s, store.ExistingRunInvocation{ID: "child"})
				if err != nil {
					t.Fatal(err)
				}
				wantStatus = store.RunStatusQueued
			case "forged":
				p = store.PreparedRunInvocation{Context: p.Context, Run: p.Run, OwnedBirth: true}
				wantStatus = store.RunStatusQueued
			case "takeover":
				r, err := fs.LoadRun(p.Context, "child")
				if err != nil {
					t.Fatal(err)
				}
				_, acquired, err := fs.AcquireRunExecutionIf(p.Context, "child", store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
				if err != nil || !acquired {
					t.Fatalf("successor acquisition = %v, %v", acquired, err)
				}
				wantErr = ErrAdmissionStopped
				wantStatus = store.RunStatusRunning
			case "metadata":
				s.beforeCAS = func(ctx context.Context, id string) error {
					r, err := fs.LoadRun(ctx, id)
					if err != nil {
						return err
					}
					r.Name = "operator-name"
					return fs.SaveRun(ctx, r)
				}
			case "format-refusal":
				cause = errors.Join(cause, store.ErrValueFormatChanged)
				wantErr = store.ErrValueFormatChanged
				wantStatus = store.RunStatusQueued
			}
			err = RetirePreparedRun(p.Context, s, p, cause)
			if !errors.Is(err, wantErr) {
				t.Fatalf("retirement = %v, want %v", err, wantErr)
			}
			r, err := fs.LoadRun(t.Context(), "child")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != wantStatus {
				t.Fatalf("status = %s, want %s", r.Status, wantStatus)
			}
			if mode == "metadata" && r.Name != "operator-name" {
				t.Fatalf("lost concurrent metadata: %q", r.Name)
			}
			if (mode == "existing" || mode == "forged" || mode == "takeover" || mode == "format-refusal") && s.writes != 0 {
				t.Fatalf("foreign/refused birth received %d writes", s.writes)
			}
		})
	}
}
