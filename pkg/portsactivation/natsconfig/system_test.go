package natsconfig

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
server_name: nats-0.example
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
	brokers, err := ObserveSystemBrokerSet(t.Context(), system, []SystemBrokerIdentity{{
		ServerID: observed.ServerID, ServerName: observed.ServerName,
	}})
	if err != nil || len(brokers) != 1 || brokers[0].ServerID != observed.ServerID {
		t.Fatalf("system PING.IDZ did not corroborate the declared broker: %+v %v", brokers, err)
	}
	if _, err := ObserveSystemBrokerSet(t.Context(), system, []SystemBrokerIdentity{{
		ServerID: observed.ServerID, ServerName: "wrong-name",
	}}); err == nil || !strings.Contains(err.Error(), "IDZ response disagrees") {
		t.Fatalf("system PING.IDZ did not reject a different declared broker name: %v", err)
	}
}

func TestNATSSystemBrokerSetRejectsAmbiguousReplies(t *testing.T) {
	declared := map[string]string{"BROKER": "known"}
	for _, raw := range []string{
		`{"name":"other","host":"127.0.0.1","id":"BROKER"}`,
		`{"name":"known","host":"127.0.0.1","id":"UNKNOWN"}`,
		`{"name":"known","host":"127.0.0.1","id":"BROKER","id":"BROKER"}`,
		`{"name":"known","host":"127.0.0.1","id":"BROKER","extra":true}`,
		`{"name":"known","host":"","id":"BROKER"}`,
	} {
		if err := recordSystemBrokerReply([]byte(raw), declared, map[string]bool{}); err == nil {
			t.Fatalf("untrusted IDZ response was accepted: %s", raw)
		}
	}
	seen := map[string]bool{}
	valid := []byte(`{"name":"known","host":"127.0.0.1","id":"BROKER"}`)
	if err := recordSystemBrokerReply(valid, declared, seen); err != nil {
		t.Fatal(err)
	}
	if err := recordSystemBrokerReply(valid, declared, seen); err == nil {
		t.Fatal("duplicate IDZ response was accepted")
	}
}

func TestNATSSystemBrokerSetDetectsUndeclaredClusterPeer(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("broker-set observation requires the pinned NATS broker")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	ports := [4]int{}
	reservations := make([]net.Listener, len(ports))
	for i := range reservations {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		reservations[i] = listener
		ports[i] = listener.Addr().(*net.TCPAddr).Port
		t.Cleanup(func() { _ = listener.Close() })
	}
	for _, listener := range reservations {
		_ = listener.Close()
	}
	clientPorts := [2]int{ports[0], ports[1]}
	routePorts := [2]int{ports[2], ports[3]}
	connections := make([]*natsclient.Conn, 2)
	for i := range connections {
		config := fmt.Sprintf(`listen: 127.0.0.1:%d
server_name: nats-%d.example
system_account: SYS
accounts { SYS {users: [{user: sys, password: fixture}]}}
cluster {
  name: TEST
  listen: 127.0.0.1:%d
  routes: ["nats-route://127.0.0.1:%d"]
}`, clientPorts[i], i, routePorts[i], routePorts[1-i])
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
		url := fmt.Sprintf("nats://127.0.0.1:%d", clientPorts[i])
		deadline := time.Now().Add(5 * time.Second)
		var err error
		for time.Now().Before(deadline) {
			connections[i], err = natsclient.Connect(url, natsclient.UserInfo("sys", "fixture"),
				natsclient.Timeout(100*time.Millisecond))
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("cluster broker %d did not start: %s", i, brokerLog.String())
		}
		defer connections[i].Close()
	}
	expected := []SystemBrokerIdentity{
		{ServerID: connections[0].ConnectedServerId(), ServerName: "nats-0.example"},
		{ServerID: connections[1].ConnectedServerId(), ServerName: "nats-1.example"},
	}
	var err error
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		_, err = ObserveSystemBrokerSet(t.Context(), connections[0], expected)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("system account did not observe both clustered brokers: %v", err)
	}
	if _, err := ObserveSystemBrokerSet(t.Context(), connections[0], expected[:1]); err == nil || !strings.Contains(err.Error(), "IDZ response disagrees") {
		t.Fatalf("system census did not detect an undeclared cluster peer: %v", err)
	}
}

func TestNATSSystemObservationRefusesUnboundBroker(t *testing.T) {
	for _, id := range []string{"", "broker.*", "broker.>"} {
		if _, err := ObserveSystemServer(t.Context(), nil, id, "sha256:"+fmt.Sprintf("%064x", 1)); err == nil {
			t.Fatalf("unbound NATS broker %q was accepted", id)
		}
	}
}

func TestNATSSystemObservationRejectsEqualTotalPageChurn(t *testing.T) {
	// Under offset pagination, removing CID 1 and adding CID 257 between
	// pages leaves both totals at 256 while CID 129 remains connected but
	// shifts across the boundary and is never observed.
	first := &systemEnvelope[systemConnz]{Server: &systemServerInfo{ID: "BROKER"},
		Data: &systemConnz{ID: "BROKER", Total: 256, NumConns: 128}}
	second := &systemEnvelope[systemConnz]{Server: &systemServerInfo{ID: "BROKER"},
		Data: &systemConnz{ID: "BROKER", Total: 256, Offset: 128, NumConns: 128}}
	for cid := uint64(1); cid <= 128; cid++ {
		first.Data.Conns = append(first.Data.Conns, systemConnzEntry{CID: cid, Account: "WORK", User: "worker"})
	}
	for cid := uint64(130); cid <= 257; cid++ {
		second.Data.Conns = append(second.Data.Conns, systemConnzEntry{CID: cid, Account: "WORK", User: "worker"})
	}
	for _, partial := range []*systemEnvelope[systemConnz]{first, second} {
		if _, err := completeSystemConnections("BROKER", partial); err == nil {
			t.Fatal("a partial CONNZ page was accepted as a complete census")
		}
	}
	complete := &systemEnvelope[systemConnz]{Server: &systemServerInfo{ID: "BROKER"},
		Data: &systemConnz{ID: "BROKER", Total: 256, NumConns: 256}}
	for cid := uint64(1); cid <= 256; cid++ {
		complete.Data.Conns = append(complete.Data.Conns, systemConnzEntry{CID: cid, Account: "WORK", User: "worker"})
	}
	connections, err := completeSystemConnections("BROKER", complete)
	if err != nil || len(connections) != 256 {
		t.Fatalf("complete single response was refused: count=%d err=%v", len(connections), err)
	}
}
