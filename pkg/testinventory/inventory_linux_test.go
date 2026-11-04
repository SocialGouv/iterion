package testinventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadCancellationStopsTheOwnedTestMain(t *testing.T) {
	for _, drain := range []string{"", "0"} {
		name := "default drain"
		if drain == "0" {
			name = "unbounded drain"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"go.mod": "module example.test/discoverychild\n\ngo 1.26\n",
				"child_test.go": `package discoverychild
import ("fmt"; "net"; "os"; "testing"; "time")
func TestMain(m *testing.M) {
 c, err := net.Dial("tcp", os.Getenv("INVENTORY_READY")); if err != nil { panic(err) }
 fmt.Fprintln(c, os.Getpid()); c.Close()
 time.Sleep(time.Hour)
 os.Exit(m.Run())
}
func TestOne(t *testing.T) {}
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(dir)
			t.Setenv("GOWORK", "off")
			t.Setenv("GOTOOLCHAIN", "local")
			t.Setenv("ITERION_PROC_WAIT_DELAY", drain)
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			// This is a fixture-start watchdog, not the cancellation verdict.
			if err := listener.SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("INVENTORY_READY", listener.Addr().String())
			ctx, cancel := context.WithCancel(context.Background())
			done, returned := make(chan error, 1), make(chan struct{})
			var child *os.Process
			t.Cleanup(func() {
				cancel()
				if child != nil {
					_ = child.Kill()
				}
				select {
				case <-returned:
				case <-time.After(time.Minute):
					t.Error("discovery did not join after its fixture was reclaimed")
				}
			})
			go func() {
				_, err := Read(ctx, ".")
				done <- err
				close(returned)
			}()
			conn, err := listener.AcceptTCP()
			if err != nil {
				t.Fatalf("TestMain did not announce its entry: %v", err)
			}
			defer conn.Close()
			if err := conn.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err := fmt.Fscan(conn, &pid); err != nil {
				t.Fatal(err)
			}
			child, err = os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			cancel() // The child's own message proves it is already in TestMain.
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("discovery cancellation = %v", err)
				}
			case <-time.After(time.Minute):
				t.Fatal("discovery kept waiting after its context was cancelled")
			}
			deadline := time.After(30 * time.Second)
			for {
				status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
				if os.IsNotExist(err) || (err == nil && bytes.Contains(status, []byte("State:\tZ"))) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-deadline:
					t.Fatalf("discovery returned but its TestMain process %d is still alive", pid)
				case <-time.After(time.Millisecond):
				}
			}
		})
	}
}
