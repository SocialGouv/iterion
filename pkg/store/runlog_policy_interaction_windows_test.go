//go:build windows

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

type closingSemanticWinIO struct {
	code     syscall.Errno
	semantic error
}

func (e closingSemanticWinIO) Error() string        { return "custom Windows IO wrapper" }
func (e closingSemanticWinIO) Is(target error) bool { return target == e.semantic }
func (e closingSemanticWinIO) Unwrap() error        { return e.code }
func TestClosingWindowsInteraction(t *testing.T) {
	ctx := context.Background()
	strict := promotedRunlogStrictContext(t, ctx)
	unknown := errors.New("future refusal")
	for _, code := range []syscall.Errno{4, 19, 29, 30, 32, 33, 39, 112, 1117, 1295} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			pure := errors.Join(&os.PathError{Err: code}, fmt.Errorf("io: %w", syscall.EIO))
			if RunLogErrorFatal(ctx, pure) {
				t.Fatal("pure IO join refused")
			}
			for _, e := range []error{pure, code} {
				if !RunLogErrorFatal(strict, e) {
					t.Fatal("strict Windows IO softened")
				}
			}
			for _, e := range []error{closingSemanticWinIO{code, unknown}, closingSemanticWinIO{code, nil}, errors.Join(code, closingSemanticWinIO{code, unknown}), errors.Join(code, unknown), errors.Join(code, context.Canceled), errors.Join(code, context.DeadlineExceeded), errors.Join(code, syscall.Errno(87)), errors.Join(code, os.ErrInvalid)} {
				if !RunLogErrorFatal(ctx, e) {
					t.Fatalf("mixed/custom/invalid softened: %v", e)
				}
			}
		})
	}
	for _, code := range []syscall.Errno{0, 87, 1816, 0x7fffffff, syscall.EINVAL} {
		if !RunLogErrorFatal(ctx, code) {
			t.Fatalf("unrecognized errno softened %d", code)
		}
	}
}
