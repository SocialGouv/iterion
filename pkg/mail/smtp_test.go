package mail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// relayScript is how a loopback relay answers the end of DATA and QUIT.
type relayScript struct {
	endOfData  string // reply to the "." that ends DATA
	dropOnQuit bool   // close the connection instead of answering QUIT
}

// startRelay serves one SMTP conversation on loopback and hands back
// the DATA payload it received.
func startRelay(t *testing.T, script relayScript) (port int, payload <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		reply := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) }
		reply("220 relay.test ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				reply("250 relay.test")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				reply("250 ok")
			case cmd == "DATA":
				reply("354 end with <CRLF>.<CRLF>")
				var body strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					body.WriteString(l)
				}
				got <- body.String()
				reply(script.endOfData)
			case cmd == "QUIT":
				if !script.dropOnQuit {
					reply("221 bye")
				}
				return
			default:
				reply("502 not implemented")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func sendThroughRelay(t *testing.T, script relayScript) (body, logs string, err error) {
	t.Helper()
	port, payload := startRelay(t, script)
	var logBuf bytes.Buffer
	m, err := NewSMTP(Config{
		Host:    "127.0.0.1",
		Port:    port,
		From:    "iterion <no-reply@example.org>",
		Timeout: 5 * time.Second,
		Logger:  iterlog.New(iterlog.LevelWarn, &logBuf),
	})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	sendErr := m.Send(context.Background(), Message{
		To:       "invitee@example.org",
		Subject:  "Join the team",
		TextBody: "accept: https://iterion.test/invite/abc",
	})
	select {
	case body = <-payload:
	case <-time.After(5 * time.Second):
	}
	return body, logBuf.String(), sendErr
}

// A relay that answered 250 to the end of DATA has the message: a QUIT
// it then drops does not turn the send into a failure.
func TestSMTPSend_aQuitDroppedAfterTheRelayTookTheMessageIsASend(t *testing.T) {
	body, logs, err := sendThroughRelay(t, relayScript{endOfData: "250 queued", dropOnQuit: true})
	if err != nil {
		t.Fatalf("Send = %v, want nil: the relay took the message before QUIT failed", err)
	}
	if !strings.Contains(body, "https://iterion.test/invite/abc") {
		t.Fatalf("relay payload lacks the link; got %q", body)
	}
	if !strings.Contains(logs, "QUIT failed") {
		t.Fatalf("the dropped QUIT is not logged; logs = %q", logs)
	}
}

func TestSMTPSend_aRelayThatRefusesTheEndOfDataFailsTheSend(t *testing.T) {
	_, _, err := sendThroughRelay(t, relayScript{endOfData: "554 rejected"})
	if err == nil || !strings.Contains(err.Error(), "finalize") {
		t.Fatalf("Send = %v, want the finalize error: the relay refused the message", err)
	}
}

func TestSMTPSend_aCleanConversationSendsAndLogsNothing(t *testing.T) {
	body, logs, err := sendThroughRelay(t, relayScript{endOfData: "250 queued"})
	if err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if !strings.Contains(body, "https://iterion.test/invite/abc") {
		t.Fatalf("relay payload lacks the link; got %q", body)
	}
	if logs != "" {
		t.Fatalf("a clean send logged %q", logs)
	}
}
