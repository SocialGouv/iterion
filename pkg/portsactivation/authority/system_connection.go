package authority

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

type systemContextDialer struct {
	ctx     context.Context
	mu      sync.Mutex
	sockets []net.Conn
}

func (d *systemContextDialer) Dial(network, address string) (net.Conn, error) {
	connection, err := (&net.Dialer{}).DialContext(d.ctx, network, address)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ctx.Err(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	d.sockets = append(d.sockets, connection)
	return connection, nil
}

func (d *systemContextDialer) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, connection := range d.sockets {
		_ = connection.Close()
	}
}

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
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := &systemContextDialer{ctx: opCtx}
	stopClose := context.AfterFunc(opCtx, dialer.closeAll)
	defer stopClose()
	connection, err := natsclient.Connect(rawURL,
		natsclient.Name("iterion-contracts-authority"), natsclient.NoReconnect(),
		natsclient.SkipHostLookup(), natsclient.SetCustomDialer(dialer), natsclient.Timeout(3*time.Second))
	if err != nil {
		if opCtx.Err() != nil {
			return nil, opCtx.Err()
		}
		return nil, fmt.Errorf("NATS system authority connection is unavailable")
	}
	// A successful connection must outlive this setup context. Stop its
	// cancellation hook before handing ownership to the caller.
	stopClose()
	if err := opCtx.Err(); err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}
