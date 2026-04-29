// Package oauth is the public façade over the internal MCP OAuth
// broker. Hosts build a *Broker once per process, then for each
// MCP server with auth_type=oauth2 call broker.BearerHeaderFunc(cfg)
// and pass the closure to TransportConfig.AuthFunc — the transport
// invokes it on every request to obtain a fresh bearer token.
package oauth

import (
	intl "github.com/SocialGouv/claw-code-go/internal/mcp/oauth"
)

type ServerConfig = intl.ServerConfig
type Token = intl.Token
type Broker = intl.Broker
type Option = intl.Option
type Storage = intl.Storage

func NewBroker(opts ...Option) *Broker      { return intl.NewBroker(opts...) }
func NewStorage(path string) *Storage       { return intl.NewStorage(path) }
func DefaultStoragePath() (string, error)   { return intl.DefaultStoragePath() }

// WithStorage routes token persistence through the given Storage.
// Default: in-memory store unique to this process.
func WithStorage(s *Storage) Option { return intl.WithStorage(s) }

// WithRedirectPort pins the loopback port for the OAuth callback.
// Default: a free port chosen at runtime.
func WithRedirectPort(port int) Option { return intl.WithRedirectPort(port) }

// WithAuthOpener overrides the function used to open the
// authorization URL in the user's browser. Default: xdg-open / open.
func WithAuthOpener(fn func(string) error) Option { return intl.WithAuthOpener(fn) }
