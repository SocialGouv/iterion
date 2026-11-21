package store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"syscall"
)

// RunLogErrorFatal preserves historical best-effort logging only for recognized
// filesystem IO failures. Every incarnation logging failure is fatal. Classification
// uses the invocation's retained binding, never a newly observed run header.
func RunLogErrorFatal(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if binding, ok := RunDataBindingFrom(ctx); ok && binding.Header().Protocol != HistoricalRunData {
		return true
	}
	return !runLogIOLeaves(err)
}

// Every leaf must be a recognized IO failure: a joined cancellation, unknown
// refusal or semantic error cannot hide behind another branch's PathError/errno.
func runLogIOLeaves(err error) bool {
	if err == nil {
		return false
	}
	// A custom Is method may carry a semantic refusal independently of its
	// wrapped IO cause. Unknown semantic wrappers fail closed; errno is the
	// recognized OS leaf whose Is method maps portable filesystem errors.
	if _, semantic := err.(interface{ Is(error) bool }); semantic {
		if _, errno := err.(syscall.Errno); !errno {
			return false
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !runLogIOLeaves(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return runLogIOLeaves(wrapped.Unwrap())
	}
	if errno, ok := err.(syscall.Errno); ok {
		switch errno {
		case syscall.EACCES, syscall.EPERM, syscall.ENOENT, syscall.EEXIST,
			syscall.ENOTDIR, syscall.EISDIR, syscall.ENOSPC, syscall.EDQUOT,
			syscall.EIO, syscall.EMFILE, syscall.ENFILE, syscall.EROFS, syscall.EBUSY:
			return true
		}
		return runLogPlatformIO(errno) || errno.Is(fs.ErrPermission) || errno.Is(fs.ErrNotExist) || errno.Is(fs.ErrExist)
	}
	return err == fs.ErrPermission || err == fs.ErrNotExist || err == fs.ErrExist || err == io.ErrShortWrite || err == io.ErrUnexpectedEOF
}
