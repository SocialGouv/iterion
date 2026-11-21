package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
	"io/fs"
	"testing"
)

type trustBindingReader struct {
	store.RunStore
	run   *store.Run
	err   error
	calls int
}

func (s *trustBindingReader) LoadRun(context.Context, string) (*store.Run, error) {
	s.calls++
	return s.run, s.err
}
func TestRunTrustBinding(t *testing.T) {
	for _, kind := range []string{"stable", "disappeared", "recreated-format", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &trustBindingReader{run: &store.Run{ID: "r22", FormatVersion: 1, Trust: store.RunTrustFork}}
			bound, trust, existing, e := cliRunTrust(ctx, s, "r22")
			if e != nil || !existing || trust != store.RunTrustFork {
				t.Fatalf("initial %v %v %v", trust, existing, e)
			}
			if e = store.RequireRunValueFormat(bound, "r22"); e != nil {
				t.Fatal(e)
			}
			var want error
			switch kind {
			case "disappeared":
				s.run = nil
				s.err = store.ErrRunNotFound
				want = store.ErrRunNotFound
			case "recreated-format":
				s.run = &store.Run{ID: "r22", FormatVersion: 2}
				want = store.ErrValueFormatChanged
			case "canceled":
				cancel()
				want = context.Canceled
			}
			got, e := store.PrepareRunInvocation(bound, s, store.ExistingRunInvocation{ID: "r22"})
			if !errors.Is(e, want) || got.OwnedBirth {
				t.Fatalf("reprepare got=%+v err=%v want=%v", got, e, want)
			}
		})
	}
}
func TestRunTrustMixedAbsence(t *testing.T) {
	for _, cause := range []error{store.ErrValueFormatChanged, context.Canceled, errors.New("infra read failure")} {
		s := &trustBindingReader{err: errors.Join(store.ErrRunNotFound, cause)}
		_, _, _, e := cliRunTrust(context.Background(), s, "r22")
		if !errors.Is(e, cause) {
			t.Errorf("mixed absence lost refusal %v: got %v", cause, e)
		}
	}
}

type trustOpaqueError struct{ err error }

func (e trustOpaqueError) Error() string        { return "redacted" }
func (e trustOpaqueError) As(target any) bool   { return errors.As(e.err, target) }
func (e trustOpaqueError) Is(target error) bool { return errors.Is(e.err, target) }

type trustManyErrors struct{ causes []error }

func (e trustManyErrors) Error() string   { return "siblings" }
func (e trustManyErrors) Unwrap() []error { return e.causes }

type trustEmptySingle struct{}

func (trustEmptySingle) Error() string { return "empty" }
func (trustEmptySingle) Unwrap() error { return nil }
func TestRunTrustAbsenceGraph(t *testing.T) {
	infra := errors.New("infrastructure")
	absent := errors.Join(store.ErrRunNotFound, &fs.PathError{Op: "open", Path: "absent", Err: fs.ErrNotExist})
	for _, row := range []struct {
		name    string
		e       error
		allowed bool
	}{
		{"realshape", absent, true},
		{"wrapped", fmt.Errorf("store: %w", absent), true},
		{"opaque-single", trustOpaqueError{fmt.Errorf("wrapped: %w", absent)}, true},
		{"opaque-mixed", trustOpaqueError{errors.Join(absent, infra)}, false},
		{"opaque-format", trustOpaqueError{errors.Join(absent, store.ErrValueFormatChanged)}, false},
		{"nil-sibling", &trustManyErrors{[]error{nil, store.ErrRunNotFound}}, true},
		{"nil-and-infra", &trustManyErrors{[]error{nil, store.ErrRunNotFound, infra}}, false},
		{"allnil", &trustManyErrors{[]error{nil, nil}}, false},
		{"emptysingle", trustEmptySingle{}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			allowed := func(e error) bool { return errors.Is(e, store.ErrRunNotFound) || errors.Is(e, fs.ErrNotExist) }
			if got := errtrack.AllLeafCauses(row.e, allowed); got != row.allowed {
				t.Fatalf("all leaves=%v want=%v", got, row.allowed)
			}
			s := &trustBindingReader{err: row.e}
			_, _, existing, e := cliRunTrust(context.Background(), s, "r23")
			if row.allowed {
				if e != nil || existing {
					t.Fatalf("pure absence rejected %v %v", existing, e)
				}
			} else if e == nil || !errors.Is(e, row.e) {
				t.Fatalf("nonabsence not preserved: %v", e)
			}
		})
	}
	t.Run("real-file-store", func(t *testing.T) {
		ctx := context.Background()
		s := store.Open(t.TempDir())
		bound, _, exists, e := cliRunTrust(ctx, s, "nonexistent")
		if e != nil || exists || bound != ctx {
			t.Fatalf("real fresh load %v %v", exists, e)
		}
	})
}
