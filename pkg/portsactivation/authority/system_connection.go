package authority

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

// DialSystem opens a dedicated, named system-account connection for read-only
// authority observations. It does not call the ordinary queue connector,
// which initializes JetStream schema. Errors never include the credential URL.
func DialSystem(ctx context.Context, rawURL string) (*natsclient.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || len(rawURL) > 2048 || strings.Contains(rawURL, ",") ||
		parsed.Scheme != "nats" && parsed.Scheme != "tls" || parsed.Hostname() == "" ||
		parsed.User == nil || parsed.User.Username() == "" || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("NATS system authority requires one authenticated broker URL")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword || password == "" {
		return nil, fmt.Errorf("NATS system authority requires URL userinfo credentials")
	}
	timeout := 3 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	connection, err := natsclient.Connect(rawURL,
		natsclient.Name("iterion-contracts-authority"), natsclient.NoReconnect(), natsclient.Timeout(timeout))
	if err != nil {
		return nil, fmt.Errorf("NATS system authority connection is unavailable")
	}
	if err := ctx.Err(); err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}
