package nats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

func TestNATSConnectionNamesDistinguishProcessRoles(t *testing.T) {
	if got := applyDefaults(Config{}).ConnectionName; got != "iterion" {
		t.Fatalf("existing caller lost its default diagnostic name: %q", got)
	}
	for _, invalid := range []string{"line\nbreak", "name.with.dot", strings.Repeat("x", 65)} {
		conn, err := Connect(t.Context(), Config{URL: "nats://127.0.0.1:1", ConnectionName: invalid})
		if err == nil || !strings.Contains(err.Error(), "invalid diagnostic connection name") {
			if conn != nil {
				conn.Close()
			}
			t.Fatalf("invalid diagnostic name %q was not rejected before dialing: %v", invalid, err)
		}
	}
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("connection-name integration requires ITERION_TEST_NATS_SERVER")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	listeners := make([]net.Listener, 2)
	for i := range listeners {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[i] = listener
	}
	port := listeners[0].Addr().(*net.TCPAddr).Port
	monitorPort := listeners[1].Addr().(*net.TCPAddr).Port
	for _, listener := range listeners {
		_ = listener.Close()
	}
	uri := fmt.Sprintf("nats://127.0.0.1:%d", port)
	monitorURL := fmt.Sprintf("http://127.0.0.1:%d/connz", monitorPort)
	command := exec.CommandContext(t.Context(), binary, "-js", "-a", "127.0.0.1",
		"-p", strconv.Itoa(port), "-m", strconv.Itoa(monitorPort), "-sd", t.TempDir())
	var brokerLog bytes.Buffer
	command.Stderr = &brokerLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probe, err := natsclient.Connect(uri, natsclient.NoReconnect(), natsclient.Timeout(100*time.Millisecond))
		if err == nil {
			probe.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("isolated NATS broker did not start: %s", brokerLog.String())
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	for _, name := range []string{"iterion-server", "iterion-runner"} {
		conn, err := Connect(t.Context(), Config{URL: uri, ConnectionName: name})
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.NATS().Opts.Name; got != name {
			conn.Close()
			t.Fatalf("NATS CONNECT lost role name: want %q, got %q", name, got)
		}
		observed := false
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			response, err := client.Get(monitorURL)
			if err == nil {
				var census struct {
					Connections []struct {
						Name string `json:"name"`
					} `json:"connections"`
				}
				decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&census)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK && decodeErr == nil {
					for _, client := range census.Connections {
						observed = observed || client.Name == name
					}
				}
			}
			if observed {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		conn.Close()
		if !observed {
			t.Fatalf("isolated broker did not report the %q client name", name)
		}
	}
}
