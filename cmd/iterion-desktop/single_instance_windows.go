//go:build desktop && windows

package main

import (
	"net"
	"path/filepath"
	"time"
)

// On Windows the IPC channel is a TCP socket on a fixed loopback port
// derived from a hash of the user profile path. Using a real named pipe
// would require golang.org/x/sys/windows + npipe; for v1 the fixed
// loopback port is good enough since flock guarantees only one owner.
const ipcWindowsPort = "127.0.0.1:38591"

func socketPath(dir string) string {
	// On Windows we still write a marker file to advertise the port —
	// future versions may switch to a named pipe and consult this file.
	return filepath.Join(dir, "iterion-desktop.sock")
}

func removeStaleSocket(_ string) error { return nil }

func listenIPC(_ string) (net.Listener, error) {
	return net.Listen("tcp", ipcWindowsPort)
}

func dialIPC(_ string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", ipcWindowsPort, timeout)
}
