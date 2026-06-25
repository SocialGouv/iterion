// Package valkey wraps a go-redis client used to share ephemeral server state
// across replicas (forge OAuth/CSRF state, board-MCP run tokens, auth
// rate-limit buckets). It supports a Sentinel-HA failover topology (the cloud
// posture) and a single-node URL (dev/local). The store implementations in
// pkg/server use the returned client directly; this package only owns
// construction + health.
package valkey

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Options configures the client. Sentinel mode wins when SentinelAddrs is set.
type Options struct {
	// URL is a single-node connection string (redis://[:pass@]host:port[/db]).
	URL string
	// SentinelAddrs is the sentinel endpoint list for HA failover mode.
	SentinelAddrs []string
	// MasterName is the Sentinel-monitored master (required with SentinelAddrs).
	MasterName string
	// Password authenticates to the data nodes.
	Password string
	// SentinelPassword authenticates to the sentinels (defaults to Password).
	SentinelPassword string
}

// Client is a thin handle over a go-redis universal client.
type Client struct {
	rdb redis.UniversalClient
}

// New builds a client. Sentinel HA when SentinelAddrs is set; otherwise a
// single-node client from URL. Errors when neither is configured.
func New(opts Options) (*Client, error) {
	switch {
	case len(opts.SentinelAddrs) > 0:
		if strings.TrimSpace(opts.MasterName) == "" {
			return nil, errors.New("valkey: master_name is required with sentinel addrs")
		}
		sentinelPass := opts.SentinelPassword
		if sentinelPass == "" {
			sentinelPass = opts.Password
		}
		return &Client{rdb: redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       opts.MasterName,
			SentinelAddrs:    opts.SentinelAddrs,
			Password:         opts.Password,
			SentinelPassword: sentinelPass,
		})}, nil
	case strings.TrimSpace(opts.URL) != "":
		o, err := redis.ParseURL(opts.URL)
		if err != nil {
			return nil, fmt.Errorf("valkey: parse url: %w", err)
		}
		if opts.Password != "" {
			o.Password = opts.Password
		}
		return &Client{rdb: redis.NewClient(o)}, nil
	default:
		return nil, errors.New("valkey: no url or sentinel addrs configured")
	}
}

// Wrap adapts an existing universal client (test seam — e.g. miniredis).
func Wrap(rdb redis.UniversalClient) *Client { return &Client{rdb: rdb} }

// Redis returns the underlying client for the store implementations.
func (c *Client) Redis() redis.UniversalClient { return c.rdb }

// Ping verifies connectivity (used by /readyz).
func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// Close releases the connection pool.
func (c *Client) Close() error { return c.rdb.Close() }
