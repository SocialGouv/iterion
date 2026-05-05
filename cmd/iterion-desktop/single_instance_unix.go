//go:build desktop && (linux || darwin || freebsd || netbsd || openbsd)

package main

import (
	"net"
	"os"
	"path/filepath"
	"time"
)

func socketPath(dir string) string {
	return filepath.Join(dir, "iterion-desktop.sock")
}

func removeStaleSocket(path string) error {
	if _, err := os.Stat(path); err == nil {
		return os.Remove(path)
	}
	return nil
}

func listenIPC(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

func dialIPC(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}
