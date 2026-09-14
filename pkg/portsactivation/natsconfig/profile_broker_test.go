package natsconfig

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/conf"
	natsclient "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func TestNATSProfileAgreesWithPinnedBroker(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("static profile differential requires the pinned NATS broker executable")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	key, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	public, err := key.PublicKey()
	if err != nil {
		t.Fatal(err)
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
  SYS { users: [{user: sys, password: fixture, permissions: {publish: [">"], subscribe: [">"]}}] }
  WORK {
    default_permissions: {publish: {allow: ["safe.>"], deny: ["safe.blocked.>"]}, subscribe: ["safe.>"]}
    users: [
      {user: inherited, password: fixture},
      {user: override, password: fixture, permissions: {}},
      {nkey: "%s", permissions: {publish: ["safe.signed"], subscribe: ["safe.signed"]}}
    ]
  }
}`, port, public)
	result, err := parseFixture(t.Context(), Sources{Entry: "main.conf", Files: map[string]string{"main.conf": config}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadProfile(result)
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
	var brokerLog bytes.Buffer // Disposable fixture values only.
	command.Stderr = &brokerLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	var observer *natsclient.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		observer, err = natsclient.Connect(url, natsclient.UserInfo("override", "fixture"), natsclient.Timeout(100*time.Millisecond))
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("static profile broker did not start: %s", brokerLog.String())
	}
	defer observer.Close()
	if observer.ConnectedServerVersion() != ParserVersion {
		t.Fatalf("profile broker version %s differs from parser %s", observer.ConnectedServerVersion(), ParserVersion)
	}
	if anonymous, err := natsclient.Connect(url, natsclient.Timeout(200*time.Millisecond)); err == nil {
		anonymous.Close()
		t.Fatal("anonymous access bypassed the named static profile")
	}
	var inherited, signed Principal
	for _, principal := range profile.Principals {
		switch principal.Identity {
		case "inherited":
			inherited = principal
		case public:
			signed = principal
		}
	}
	for _, tc := range []struct {
		name      string
		options   []natsclient.Option
		principal Principal
		subjects  []string
	}{
		{"password default", []natsclient.Option{natsclient.UserInfo("inherited", "fixture")}, inherited,
			[]string{"safe.one", "safe.blocked.one", "$KV.iterion-runner-rollout.ports-v1.census.runner.pod"}},
		{"public nkey", []natsclient.Option{natsclient.Nkey(public, key.Sign)}, signed,
			[]string{"safe.signed", "safe.other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := natsclient.Connect(url, tc.options...)
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()
			for _, subject := range tc.subjects {
				publishAllowed, err := tc.principal.Publish.Allows(subject)
				if err != nil {
					t.Fatal(err)
				}
				receiver, err := observer.SubscribeSync(subject)
				if err != nil {
					t.Fatal(err)
				}
				if err := observer.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				body := []byte(tc.name + subject)
				if err := nc.Publish(subject, body); err != nil {
					t.Fatal(err)
				}
				if err := nc.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				message, receiveErr := receiver.NextMsg(80 * time.Millisecond)
				_ = receiver.Unsubscribe()
				if got := receiveErr == nil && bytes.Equal(message.Data, body); got != publishAllowed {
					t.Errorf("publish %s: broker=%t profile=%t", subject, got, publishAllowed)
				}
				subscribeAllowed, err := tc.principal.Subscribe.Allows(subject)
				if err != nil {
					t.Fatal(err)
				}
				sub, err := nc.SubscribeSync(subject)
				if err != nil {
					t.Fatal(err)
				}
				if err := nc.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				if err := observer.Publish(subject, body); err != nil {
					t.Fatal(err)
				}
				if err := observer.FlushTimeout(time.Second); err != nil {
					t.Fatal(err)
				}
				message, receiveErr = sub.NextMsg(80 * time.Millisecond)
				_ = sub.Unsubscribe()
				if got := receiveErr == nil && bytes.Equal(message.Data, body); got != subscribeAllowed {
					t.Errorf("subscribe %s: broker=%t profile=%t", subject, got, subscribeAllowed)
				}
			}
		})
	}
}

func TestNATSConfigRejectsLossyParsedSubjects(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("lossy-subject differential requires the pinned NATS broker executable")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	// The source is UTF-8, but the upstream NATS lexer decodes the ASCII
	// \xFF escape into an invalid byte. JSON would replace that byte with
	// U+FFFD and collapse these two different permission subjects.
	config := fmt.Sprintf(`listen: 127.0.0.1:%d
jetstream: true
system_account: SYS
accounts {
  SYS {users: [{user: sys, password: fixture}]}
  WORK {users: [
    {user: observer, password: fixture, permissions: {}},
    {user: restricted, password: fixture, permissions: {publish: {allow: ["safe.�"], deny: ["safe.\xFF"]}}}
  ]}
}`, port)
	if err := (Sources{Entry: "main.conf", Files: map[string]string{"main.conf": config}}).Validate(); err != nil {
		t.Fatalf("fixture source itself must be valid UTF-8: %v", err)
	}
	if _, err := parseFixture(t.Context(), Sources{Entry: "main.conf", Files: map[string]string{"main.conf": config}}); err == nil {
		t.Fatal("lossy upstream permission escape was projected through JSON")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "main.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, _, err := conf.ParseFileWithChecksDigest(configPath)
	if err != nil {
		t.Fatalf("pinned upstream parser should accept the byte escape: %v", err)
	}
	if err := validateParsedUTF8(parsed); err == nil {
		t.Fatal("parsed non-UTF-8 permission was not detected before JSON encoding")
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
		t.Fatalf("lossy-subject broker did not start: %s", brokerLog.String())
	}
	defer observer.Close()
	restricted, err := natsclient.Connect(url, natsclient.UserInfo("restricted", "fixture"),
		natsclient.ErrorHandler(func(*natsclient.Conn, *natsclient.Subscription, error) {}))
	if err != nil {
		t.Fatal(err)
	}
	defer restricted.Close()
	for _, tc := range []struct {
		subject string
		want    bool
	}{
		{"safe.�", true},
		{"safe." + string([]byte{0xff}), false},
	} {
		receiver, err := observer.SubscribeSync(tc.subject)
		if err != nil {
			t.Fatal(err)
		}
		if err := observer.FlushTimeout(time.Second); err != nil {
			t.Fatal(err)
		}
		body := []byte("permission byte distinction")
		if err := restricted.Publish(tc.subject, body); err != nil {
			t.Fatal(err)
		}
		if err := restricted.FlushTimeout(time.Second); err != nil {
			t.Fatal(err)
		}
		message, receiveErr := receiver.NextMsg(100 * time.Millisecond)
		_ = receiver.Unsubscribe()
		if got := receiveErr == nil && bytes.Equal(message.Data, body); got != tc.want {
			t.Errorf("broker and JSON projection disagree for UTF-8 vs byte escape: got %t, want %t", got, tc.want)
		}
	}
}
