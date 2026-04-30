package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	clawmcp "github.com/SocialGouv/claw-code-go/pkg/api/mcp"
	clawoauth "github.com/SocialGouv/claw-code-go/pkg/api/mcp/oauth"
)

// AuthFunc is the closure shape SSE/HTTP transports invoke on every
// outbound request to fetch a fresh "Authorization" header value.
type AuthFunc = func(ctx context.Context) (string, error)

// OAuthBroker is the iterion-side wrapper around claw's OAuth broker.
// It owns the persistent token cache (storage path lives under the
// run's store dir so a workflow restart can reuse refresh tokens) and
// hands out per-server BearerHeaderFunc closures.
//
// The zero value is not usable; construct via NewOAuthBroker.
//
// Concurrency: the underlying clawoauth.Broker is safe for concurrent
// use; this wrapper holds no mutable state of its own and adds no
// further locking.
type OAuthBroker struct {
	broker  *clawoauth.Broker
	storage *clawoauth.Storage
}

// NewOAuthBroker builds an OAuth broker rooted at the given store
// directory. Tokens are persisted under <storeDir>/mcp_oauth.json so
// the same broker can be re-instantiated by a later run.
//
// Pass an empty storeDir to fall back to the platform default
// (claw's $XDG_DATA_HOME path).
func NewOAuthBroker(storeDir string) (*OAuthBroker, error) {
	var storage *clawoauth.Storage
	if storeDir != "" {
		storage = clawoauth.NewStorage(filepath.Join(storeDir, "mcp_oauth.json"))
	} else {
		path, err := clawoauth.DefaultStoragePath()
		if err != nil {
			return nil, fmt.Errorf("oauth: resolve default storage path: %w", err)
		}
		storage = clawoauth.NewStorage(path)
	}
	br := clawoauth.NewBroker(clawoauth.WithStorage(storage))
	return &OAuthBroker{broker: br, storage: storage}, nil
}

// AuthFuncFor returns a closure that resolves to a "Bearer <token>"
// header for an SSE/HTTP MCP transport. The closure is safe to call
// concurrently; the broker handles refresh and storage.
//
// Returns an error if cfg is missing required fields (Type, AuthURL,
// TokenURL, ClientID).
func (b *OAuthBroker) AuthFuncFor(cfg *AuthConfig, serverName string) (AuthFunc, error) {
	if cfg == nil {
		return nil, errors.New("oauth: nil AuthConfig")
	}
	if cfg.Type != "oauth2" {
		return nil, fmt.Errorf("oauth: unsupported auth type %q (only oauth2 is wired)", cfg.Type)
	}
	if cfg.AuthURL == "" || cfg.TokenURL == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("oauth: server %q config missing AuthURL/TokenURL/ClientID", serverName)
	}
	for _, ep := range []struct{ name, value string }{
		{"AuthURL", cfg.AuthURL},
		{"TokenURL", cfg.TokenURL},
		{"RevokeURL", cfg.RevokeURL},
	} {
		if ep.value == "" {
			continue
		}
		if err := requireSecureURL(ep.value); err != nil {
			return nil, fmt.Errorf("oauth: server %q %s: %w", serverName, ep.name, err)
		}
	}
	srv := clawoauth.ServerConfig{
		Name:      serverName,
		AuthURL:   cfg.AuthURL,
		TokenURL:  cfg.TokenURL,
		RevokeURL: cfg.RevokeURL,
		ClientID:  cfg.ClientID,
		Scopes:    append([]string(nil), cfg.Scopes...),
	}
	return b.broker.BearerHeaderFunc(srv), nil
}

// AuthStatus reports the persisted OAuth token state for `serverName`.
// Returns "connected" when a non-expired token is on file, "auth_required"
// when a token is missing, and "expired" when the cache holds a stale
// token. The returned struct keeps Server/Type/Scopes/ExpiresAt fields so
// the mcp_auth tool can format a complete report.
func (b *OAuthBroker) AuthStatus(serverName string) clawmcp.ServerStatus {
	if b == nil || b.storage == nil {
		return clawmcp.ServerStatus{Name: serverName, Status: "disconnected"}
	}
	tok, ok, err := b.storage.Load(serverName)
	switch {
	case err != nil:
		return clawmcp.ServerStatus{Name: serverName, Status: "error", ServerInfo: err.Error()}
	case !ok:
		return clawmcp.ServerStatus{Name: serverName, Status: "auth_required"}
	case tok.IsExpired(30 * time.Second):
		return clawmcp.ServerStatus{Name: serverName, Status: "expired"}
	default:
		info := "oauth2"
		if tok.Scope != "" {
			info = info + " (scope=" + tok.Scope + ")"
		}
		return clawmcp.ServerStatus{Name: serverName, Status: "connected", ServerInfo: info}
	}
}

// requireSecureURL accepts any https:// URL and rejects http:// unless
// the host is a loopback address (localhost / 127.0.0.0/8 / [::1]).
// PKCE-loopback redirect targets and on-host token endpoints during
// local development legitimately use http://localhost.
func requireSecureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.") {
			return nil
		}
		return fmt.Errorf("non-loopback http:// URL not allowed for OAuth (got %q); use https://", raw)
	default:
		return fmt.Errorf("unsupported URL scheme %q (need https or http://localhost)", u.Scheme)
	}
}
