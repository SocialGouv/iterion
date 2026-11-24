//go:build windows

package store

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

// Win32 values verified in the repository's vendor/golang.org/x/sys/windows/zerrors_windows.go.
func TestWindowsNativeIOCodes(t *testing.T) {
	for name, code := range map[string]syscall.Errno{"too-many-open-files": 4, "write-protect": 19, "sharing-violation": 32, "handle-disk-full": 39, "disk-full": 112, "io-device": 1117} {
		fatal := RunLogErrorFatal(context.Background(), code)
		t.Logf("%s native-code=%d POSIX-ENOSPC=%d fatal=%v", name, code, syscall.ENOSPC, fatal)
		if fatal {
			t.Errorf("historical ordinary Win32 IO made fatal: %s", name)
		}
	}
	if !RunLogErrorFatal(context.Background(), syscall.Errno(87)) {
		t.Fatal("invalid parameter softened")
	}
}
func TestWindowsMappedIOKeepsStrictAndRefusals(t *testing.T) {
	ctx := context.Background()
	strict := promotedRunlogStrictContext(t, ctx)
	for _, code := range []syscall.Errno{4, 19, 29, 30, 32, 33, 39, 112, 1117, 1295} {
		if RunLogErrorFatal(ctx, code) {
			t.Errorf("historical IO fatal %d", code)
		}
		if !RunLogErrorFatal(strict, code) {
			t.Errorf("strict IO softened %d", code)
		}
		if !RunLogErrorFatal(ctx, errors.Join(code, errors.New("unknown refusal"))) {
			t.Errorf("mixed semantic softened %d", code)
		}
		if !RunLogErrorFatal(ctx, errors.Join(code, context.Canceled)) {
			t.Errorf("mixed cancel softened %d", code)
		}
	}
	for _, code := range []syscall.Errno{87, 1816, 0, 0x7fffffff, syscall.EINVAL} {
		if !RunLogErrorFatal(ctx, code) {
			t.Errorf("unknown/invalid widened %d", code)
		}
	}
}

// No storage admission is needed to classify an already-captured binding.
func promotedRunlogStrictContext(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	b, err := CaptureRunDataBinding(RunDataHeader{RunID: "runlog-policy", Format: ExactValueFormat, Protocol: IncarnationRunData, BirthID: "original-birth"})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := WithRunDataBinding(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}
