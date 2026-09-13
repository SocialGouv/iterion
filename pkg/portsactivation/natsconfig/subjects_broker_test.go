package natsconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

func TestNATSPermissionsAgreeWithPinnedBroker(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("permission differential test requires ITERION_TEST_NATS_SERVER")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	cases := []struct {
		name       string
		configured any
		expected   SubjectPermissions
	}{
		{"inherit-default", nil, SubjectPermissions{Deny: []string{">"}}},
		{"explicit-empty-overrides-default", map[string]any{}, SubjectPermissions{}},
		{"empty-list-is-unrestricted", []string{}, SubjectPermissions{Allow: []string{}}},
		{"safe-only", "safe.>", SubjectPermissions{Allow: []string{"safe.>"}}},
		{"wildcard-minus-deny", map[string]any{"allow": []string{">"}, "deny": []string{"$JS.API.CONSUMER.>", "safe.blocked.>"}}, SubjectPermissions{Allow: []string{">"}, Deny: []string{"$JS.API.CONSUMER.>", "safe.blocked.>"}}},
		{"deny-union", map[string]any{"allow": []string{"safe.>"}, "deny": []string{"safe.*", "safe.*.>"}}, SubjectPermissions{Allow: []string{"safe.>"}, Deny: []string{"safe.*", "safe.*.>"}}},
		{"protected-family", map[string]any{"allow": []string{"$KV.iterion-runner-rollout.>"}}, SubjectPermissions{Allow: []string{"$KV.iterion-runner-rollout.>"}}},
	}
	users := []any{map[string]any{"user": "observer", "password": "fixture", "permissions": map[string]any{}}}
	for _, tc := range cases {
		user := map[string]any{"user": tc.name, "password": "fixture"}
		if tc.configured != nil {
			// The explicit empty permissions block overrides account defaults.
			// For every other case publish and subscribe use identical rules.
			if tc.name == "explicit-empty-overrides-default" {
				user["permissions"] = tc.configured
			} else {
				user["permissions"] = map[string]any{"publish": tc.configured, "subscribe": tc.configured}
			}
		}
		users = append(users, user)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	var configBody bytes.Buffer
	encoder := json.NewEncoder(&configBody)
	// NATS accepts JSON-style maps but not JSON's Unicode escape for >.
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", port), "accounts": map[string]any{"WORK": map[string]any{
		"users": users, "default_permissions": map[string]any{"publish": map[string]any{"deny": []string{">"}}, "subscribe": map[string]any{"deny": []string{">"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "main.conf")
	if err := os.WriteFile(config, configBody.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-c", config)
	var brokerLog bytes.Buffer // Only the static, disposable fixture configuration.
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
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("permission fixture broker did not start: %s", brokerLog.String())
	}
	defer observer.Close()
	if observer.ConnectedServerVersion() != ParserVersion {
		t.Fatalf("permission test broker version %s differs from parser %s", observer.ConnectedServerVersion(), ParserVersion)
	}
	baseSubjects := []string{
		"$JS.API.CONSUMER.MSG.NEXT.ITERION_RUNS.iterion-runners",
		"$JS.API.STREAM.UPDATE.ITERION_RUNS", "$JS.API.DIRECT.GET.ITERION_RUNS",
		"$JS.ACK.ITERION_RUNS.iterion-runners.1.1.1.1.1", "$KV.iterion-run-locks.pc1_fixture",
		"$KV.iterion-runner-rollout.ports-v1.census.runner.pod", "safe.one", "safe.blocked", "safe.blocked.deep", "safe", "other",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := natsclient.Connect(url, natsclient.UserInfo(tc.name, "fixture"), natsclient.ErrorHandler(func(*natsclient.Conn, *natsclient.Subscription, error) {}))
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()
			subjects := slices.Clone(baseSubjects)
			witness, found, err := tc.expected.Intersects([]string{"$JS.API.>", "$JS.ACK.>", "$KV.iterion-run-locks.>", "$KV.iterion-runner-rollout.>"})
			if err != nil {
				t.Fatal(err)
			}
			if found && !slices.Contains(subjects, witness) {
				subjects = append(subjects, witness)
			}
			for _, subject := range subjects {
				allowed, err := tc.expected.Allows(subject)
				if err != nil {
					t.Fatal(err)
				}
				probe := []byte(tc.name + subject)
				// Observe actual publish delivery, including the protected API
				// and KV subjects. This broker has no data streams to mutate.
				receiver, err := observer.SubscribeSync(subject)
				if err != nil {
					t.Fatal(err)
				}
				if err := observer.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				if err := nc.Publish(subject, probe); err != nil {
					t.Fatal(err)
				}
				if err := nc.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				msg, receiveErr := receiver.NextMsg(60 * time.Millisecond)
				_ = receiver.Unsubscribe()
				got := receiveErr == nil && bytes.Equal(msg.Data, probe)
				if got != allowed {
					t.Errorf("publish %s: broker=%t evaluator=%t (receive error: %v)", subject, got, allowed, receiveErr)
				}
				// Check concrete subscription delivery separately. Wildcard
				// subscriptions may be accepted and filtered at delivery time.
				sub, err := nc.SubscribeSync(subject)
				if err != nil {
					t.Fatal(err)
				}
				if err := nc.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				if err := observer.Publish(subject, probe); err != nil {
					t.Fatal(err)
				}
				if err := observer.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				msg, receiveErr = sub.NextMsg(60 * time.Millisecond)
				_ = sub.Unsubscribe()
				got = receiveErr == nil && bytes.Equal(msg.Data, probe)
				if got != allowed {
					t.Errorf("subscribe %s: broker=%t evaluator=%t (receive error: %v)", subject, got, allowed, receiveErr)
				}
			}
		})
	}
}
