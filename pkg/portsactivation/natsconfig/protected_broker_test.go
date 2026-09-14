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

	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
	natsclient "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestNATSProtectedAccessMatchesPinnedJetStream(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("protected queue differential requires the pinned NATS broker executable")
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
  WORK {jetstream: true, users: [
    {user: observer, password: fixture, permissions: {}},
    {user: fetcher, password: fixture, permissions: {publish: ["$JS.API.CONSUMER.MSG.NEXT.ITERION_RUNS.iterion-runners"], subscribe: ["_INBOX.>"]}},
    {user: purger, password: fixture, permissions: {publish: ["$JS.API.STREAM.PURGE.ITERION_RUNS"], subscribe: ["_INBOX.>"]}},
    {user: reader, password: fixture, permissions: {publish: ["safe.>"], subscribe: ["iterion.queue.runs"]}}
  ]}
}`, port)
	parsed, err := parseFixture(t.Context(), Sources{Entry: "main.conf", Files: map[string]string{"main.conf": config}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadProfile(parsed)
	if err != nil {
		t.Fatal(err)
	}
	principals := make(map[string]Principal)
	for _, principal := range profile.Principals {
		principals[principal.Identity] = principal
	}
	topology := protectedFixture()
	for _, tc := range []struct {
		identity string
		surface  string
	}{
		{"fetcher", "jetstream_api"},
		{"purger", "jetstream_api"},
		{"reader", "queue_messages"},
		{"observer", "unreviewed_jetstream_api"},
		{"sys", "system_authority"},
	} {
		exposures, err := AnalyzeProtectedAccess(principals[tc.identity], topology)
		if err != nil || !hasExposure(exposures, tc.surface) {
			t.Fatalf("%s capability missing from profile: %+v %v", tc.identity, exposures, err)
		}
		if tc.identity == "fetcher" || tc.identity == "purger" {
			if hasExposure(exposures, "unreviewed_jetstream_api") {
				t.Fatalf("exact API grant was classified as an unknown API: %+v", exposures)
			}
		}
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
	var observer *natsclient.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		observer, err = natsclient.Connect(url, natsclient.UserInfo("observer", "fixture"), natsclient.Timeout(100*time.Millisecond))
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("protected queue broker did not start: %s", brokerLog.String())
	}
	defer observer.Close()
	if observer.ConnectedServerVersion() != ParserVersion {
		t.Fatalf("protected queue broker version %s differs from parser %s", observer.ConnectedServerVersion(), ParserVersion)
	}
	js, err := jetstream.New(observer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: queue.StreamRuns, Subjects: []string{queue.SubjectRuns}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = js.CreateConsumer(ctx, queue.StreamRuns, jetstream.ConsumerConfig{
		Durable: queue.ConsumerRunners, AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: queue.SubjectRuns,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, queue.SubjectRuns, []byte("queued native run")); err != nil {
		t.Fatal(err)
	}
	fetcher, err := natsclient.Connect(url, natsclient.UserInfo("fetcher", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer fetcher.Close()
	msg, err := fetcher.Request("$JS.API.CONSUMER.MSG.NEXT."+queue.StreamRuns+"."+queue.ConsumerRunners,
		[]byte(`{"batch":1,"expires":1000000000}`), time.Second)
	if err != nil || !bytes.Equal(msg.Data, []byte("queued native run")) {
		t.Fatalf("exact fetch permission did not receive shared durable delivery: %+v %v", msg, err)
	}
	reader, err := natsclient.Connect(url, natsclient.UserInfo("reader", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	sub, err := reader.SubscribeSync(queue.SubjectRuns)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.FlushTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := observer.Publish(queue.SubjectRuns, []byte("core eavesdrop")); err != nil {
		t.Fatal(err)
	}
	if err := observer.FlushTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	core, err := sub.NextMsg(time.Second)
	if err != nil || !bytes.Equal(core.Data, []byte("core eavesdrop")) {
		t.Fatalf("core queue subscriber did not see the run subject: %+v %v", core, err)
	}
	purger, err := natsclient.Connect(url, natsclient.UserInfo("purger", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer purger.Close()
	response, err := purger.Request("$JS.API.STREAM.PURGE."+queue.StreamRuns, []byte(`{}`), time.Second)
	if err != nil || !bytes.Contains(response.Data, []byte(`"success":true`)) {
		t.Fatalf("exact stream purge permission did not mutate the queue: %+v %v", response, err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 0 {
		t.Fatalf("purge did not clear the shared stream: %+v %v", info, err)
	}
}
