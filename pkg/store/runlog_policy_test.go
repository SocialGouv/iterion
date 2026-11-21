package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestR53RunLogErrorPolicy(t *testing.T) {
	for _, p := range []RunDataProtocol{0, 1} {
		b, e := CaptureRunDataBinding(RunDataHeader{RunID: "r", Format: 2, Protocol: p, BirthID: "birth"})
		if e != nil {
			t.Fatal(e)
		}
		c, e := WithRunDataBinding(context.Background(), b)
		if e != nil {
			t.Fatal(e)
		}
		unknown := errors.New("future semantic refusal")
		ioerr := &os.PathError{Op: "open", Path: "log", Err: syscall.EISDIR}
		for _, tc := range []struct {
			name string
			err  error
			io   bool
		}{{"nil", nil, true}, {"io", ioerr, true}, {"wrappedIO", fmt.Errorf("sink: %w", ioerr), true}, {"allIOJoin", errors.Join(ioerr, syscall.ENOSPC), true}, {"unknown", unknown, false}, {"unknownPath", &os.PathError{Err: unknown}, false}, {"cancelPath", &os.PathError{Err: context.Canceled}, false}, {"deadlineJoin", errors.Join(ioerr, context.DeadlineExceeded), false}, {"identityJoin", errors.Join(ioerr, ErrRunDataIdentityChanged), false}, {"unknownMixed", errors.Join(ioerr, unknown), false}, {"nestedMixed", fmt.Errorf("outer: %w", errors.Join(ioerr, &os.SyscallError{Syscall: "open", Err: unknown})), false}, {"invalid", syscall.EINVAL, false}, {"osInvalid", os.ErrInvalid, false}} {
			t.Run(fmt.Sprintf("p%d/%s", p, tc.name), func(t *testing.T) {
				want := tc.err != nil && (p == 1 || !tc.io)
				if got := RunLogErrorFatal(c, tc.err); got != want {
					t.Fatalf("fatal=%v want%v: %v", got, want, tc.err)
				}
			})
		}
		cancelled, cancel := context.WithCancel(c)
		cancel()
		if !RunLogErrorFatal(cancelled, ioerr) {
			t.Fatal("canceled IO softened")
		}
	}
}
