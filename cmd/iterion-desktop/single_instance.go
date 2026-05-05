//go:build desktop

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// SingleInstance enforces exactly-one-running iterion-desktop. The lock is
// flock(2) on a file in the user config dir; a sibling Unix socket /
// Windows TCP loopback acts as the IPC channel that secondary launches
// use to ask the running owner to surface its window.
type SingleInstance struct {
	lock     *flock.Flock
	listener net.Listener
	stop     chan struct{}
	// releaseOnce makes Release idempotent. The previous implementation
	// called close(stop) unconditionally and panicked on a second call,
	// contradicting the documented "Safe to call multiple times" contract.
	releaseOnce sync.Once
}

func instanceDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(dir, "Iterion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// AcquireSingleInstanceLock attempts to grab the exclusive flock. Returns
// an error if the file is already locked (i.e. another instance is
// running).
func AcquireSingleInstanceLock() (*SingleInstance, error) {
	dir, err := instanceDir()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "iterion-desktop.lock")
	lock := flock.New(lockPath)
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("another instance is already running")
	}
	return &SingleInstance{lock: lock, stop: make(chan struct{})}, nil
}

// Listen starts accepting IPC connections in a goroutine. Each accepted
// connection triggers `onFocus` (which the desktop App wires to
// WindowShow + WindowUnminimise). Errors are ignored — the lock is still
// the source of truth, IPC is best-effort.
func (s *SingleInstance) Listen(onFocus func()) {
	dir, err := instanceDir()
	if err != nil {
		return
	}
	sockPath := socketPath(dir)
	// Best-effort cleanup of a stale socket from a previous crash.
	_ = removeStaleSocket(sockPath)

	ln, err := listenIPC(sockPath)
	if err != nil {
		return
	}
	s.listener = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-s.stop:
					return
				default:
					return
				}
			}
			// We don't read anything — the mere arrival of a connection
			// is the focus signal.
			_ = conn.Close()
			if onFocus != nil {
				onFocus()
			}
		}
	}()
}

// Release drops the lock and stops the listener. Safe to call multiple
// times — channel-close, listener-close, and flock-unlock are all guarded
// by sync.Once so a double call (e.g. from a `defer` plus the explicit
// onShutdown call, or onShutdown firing twice on some Wails teardown
// paths) cannot panic with "close of closed channel".
func (s *SingleInstance) Release() error {
	if s == nil {
		return nil
	}
	var unlockErr error
	s.releaseOnce.Do(func() {
		close(s.stop)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.lock != nil {
			unlockErr = s.lock.Unlock()
		}
	})
	return unlockErr
}

// SignalExistingInstance opens a connection to the listening owner socket;
// a single open-and-close is treated by the owner as "focus me". 2-second
// timeout so a stale socket can't hang the secondary launcher.
func SignalExistingInstance() error {
	dir, err := instanceDir()
	if err != nil {
		return err
	}
	conn, err := dialIPC(socketPath(dir), 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}
