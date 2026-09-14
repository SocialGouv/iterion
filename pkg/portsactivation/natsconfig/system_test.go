package natsconfig

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

func TestNATSSystemObservationMatchesPinnedBroker(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("system observation requires the pinned NATS broker executable")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	config := fmt.Sprintf(`listen: 127.0.0.1:%d
jetstream: true
system_account: SYS
accounts {
  SYS {users: [{user: sys, password: fixture}]}
  WORK {jetstream: true, users: [{user: worker, password: fixture}]}
}`, port)
	parsed, err := parseFixture(t.Context(), Sources{Entry: "main.conf", Files: map[string]string{"main.conf": config}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "main.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-c", configPath)
	command.Dir = dir
	var brokerLog bytes.Buffer
	command.Stderr = &brokerLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	var system *natsclient.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		system, err = natsclient.Connect(url, natsclient.UserInfo("sys", "fixture"), natsclient.Timeout(100*time.Millisecond))
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("system observation broker did not start: %s", brokerLog.String())
	}
	defer system.Close()
	worker, err := natsclient.Connect(url, natsclient.UserInfo("worker", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	observed, err := ObserveSystemServer(ctx, system, system.ConnectedServerId(), parsed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ServerID != system.ConnectedServerId() || observed.Version != ParserVersion ||
		observed.ConfigDigest != parsed.Digest {
		t.Fatalf("system observation disagreed with source and connected broker: %+v", observed)
	}
	foundSystem, foundWorker := false, false
	for _, connection := range observed.Connections {
		foundSystem = foundSystem || connection.Account == "SYS" && connection.User == "sys"
		foundWorker = foundWorker || connection.Account == "WORK" && connection.User == "worker"
	}
	if !foundSystem || !foundWorker {
		t.Fatalf("system CONNZ omitted authenticated holders: %+v", observed.Connections)
	}
	if _, err := ObserveSystemServer(ctx, system, system.ConnectedServerId(), "sha256:"+fmt.Sprintf("%064x", 1)); err == nil {
		t.Fatal("mismatched loaded configuration digest was accepted")
	}
}

func TestNATSSystemObservationRefusesUnboundBroker(t *testing.T) {
	for _, id := range []string{"", "broker.*", "broker.>"} {
		if _, err := ObserveSystemServer(t.Context(), nil, id, "sha256:"+fmt.Sprintf("%064x", 1)); err == nil {
			t.Fatalf("unbound NATS broker %q was accepted", id)
		}
	}
}
