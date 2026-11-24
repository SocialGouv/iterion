package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

type independentSemanticIO struct{ semantic error }

func (e independentSemanticIO) Error() string        { return "semantic refusal with IO cause" }
func (e independentSemanticIO) Is(target error) bool { return target == e.semantic }
func (e independentSemanticIO) Unwrap() error        { return syscall.EIO }
func TestIndependentR53RunLogSemanticPriority(t *testing.T) {
	for _, sentinel := range []error{ErrRunDataIdentityChanged, ErrRunDataBindingRequired, ErrInvalidRunDataIdentity, ErrUnsupportedRunDataProtocol, ErrRunDataCapability, ErrValueFormatChanged, ErrUnsupportedValueFormat, ErrInvalidValueFormat, ErrValueFormatCapability, ErrRunDeleted, fs.ErrInvalid, context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			e := independentSemanticIO{sentinel}
			if !errors.Is(e, sentinel) {
				t.Fatal("broken fixture")
			}
			for _, wrapped := range []error{e, fmt.Errorf("outer: %w", e), &os.PathError{Op: "open", Path: "run.log", Err: e}, errors.Join(syscall.EACCES, e)} {
				if !RunLogErrorFatal(context.Background(), wrapped) {
					t.Errorf("semantic refusal softened: %T %v", wrapped, sentinel)
				}
			}
		})
	}
}
func TestIndependentR53RunLogMixedUnknown(t *testing.T) {
	unknown := errors.New("new refusal")
	for _, e := range []error{unknown, fmt.Errorf("wrapped: %w", unknown), errors.Join(&os.PathError{Err: syscall.ENOENT}, unknown), &os.SyscallError{Syscall: "open", Err: errors.Join(syscall.EIO, syscall.EINVAL)}, errors.Join(syscall.EIO, &os.LinkError{Err: ErrRunDeleted})} {
		if !RunLogErrorFatal(context.Background(), e) {
			t.Fatal("unknown/invalid softened", e)
		}
	}
	for _, e := range []error{syscall.EACCES, fmt.Errorf("outer: %w", &os.PathError{Err: syscall.ENOSPC}), errors.Join(syscall.EIO, syscall.EACCES)} {
		if RunLogErrorFatal(context.Background(), e) {
			t.Fatal("legacy IO made fatal", e)
		}
	}
}

type independentOpaqueIO struct{}

func (independentOpaqueIO) Error() string { return "unknown wrapper" }
func (independentOpaqueIO) Is(error) bool { return false }
func (independentOpaqueIO) Unwrap() error { return syscall.EIO }
func TestIndependentR53OpaqueIsFailsClosed(t *testing.T) {
	if !RunLogErrorFatal(context.Background(), independentOpaqueIO{}) {
		t.Fatal("unknown semantic wrapper softened")
	}
	if RunLogErrorFatal(context.Background(), errors.Join(syscall.EIO, syscall.EACCES)) {
		t.Fatal("pure errno join refused")
	}
}
