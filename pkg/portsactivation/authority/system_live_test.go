package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
	natsclient "github.com/nats-io/nats.go"
)

func TestAuthorityLiveSystemCorroboratesNamedBroker(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("live authority observation requires the pinned NATS broker")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	currentBinary := os.Getenv("ITERION_TEST_CURRENT_BINARY")
	if currentBinary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("live authority observation requires the current production parser binary")
		}
		t.Skip("ITERION_TEST_CURRENT_BINARY not set")
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
  SYS {users: [{user: sys, password: fixture, permissions: {publish: ["$SYS.REQ.>"], subscribe: ["_INBOX.>"]}}]}
  WORK {jetstream: true, users: [{user: worker-user, password: fixture, permissions: {publish: ["$JS.API.CONSUMER.MSG.NEXT.%s.%s", "$JS.ACK.>"], subscribe: ["_INBOX.>"]}}]}
}`, port, queue.StreamRuns, queue.ConsumerRunners)
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
		t.Fatalf("live authority broker did not start: %s", brokerLog.String())
	}
	defer system.Close()
	worker, err := natsclient.Connect(url, natsclient.UserInfo("worker-user", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	record := staticFixture(t)
	serverID := system.ConnectedServerId()
	record.Brokers[0].ServerID, record.Brokers[0].ServerName = serverID, "nats-0.example"
	record.Brokers[0].sources = natsconfig.Sources{Entry: "main.conf", Files: map[string]string{"main.conf": config}}
	static, err := analyzeStaticWithParser(t.Context(), record,
		func(ctx context.Context, sources natsconfig.Sources) (*natsconfig.Result, error) {
			input, err := json.Marshal(sources)
			if err != nil {
				return nil, err
			}
			parser := exec.CommandContext(ctx, currentBinary, natsconfig.HelperCommand)
			parser.Env = []string{}
			parser.Stdin = bytes.NewReader(input)
			output, err := parser.Output()
			if err != nil {
				return nil, err
			}
			var result natsconfig.Result
			if err := json.Unmarshal(output, &result); err != nil {
				return nil, err
			}
			return &result, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ObserveAndCorroborateSystem(t.Context(), system, record, static)
	if err != nil || len(result.Brokers) != 1 || result.Brokers[0].ServerID != serverID ||
		result.Brokers[0].ObservedConnections != 2 {
		t.Fatalf("live authority did not bind broker and connected principals: %+v %v", result, err)
	}
	static.Brokers[0].ConfigDigest = "sha256:" + strings.Repeat("f", 64)
	if _, err := ObserveAndCorroborateSystem(t.Context(), system, record, static); err == nil {
		t.Fatal("live authority accepted a stale parsed configuration digest")
	}
}
