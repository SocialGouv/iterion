package api

import (
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// AuthSourceKind discriminates AuthSource variants.
type AuthSourceKind = api.AuthSourceKind

// AuthSource holds authentication credentials for API requests.
type AuthSource = api.AuthSource

// AuthMethod describes how a provider authenticates.
type AuthMethod = api.AuthMethod

const (
	AuthSourceNone     = api.AuthSourceNone
	AuthSourceAPIKey   = api.AuthSourceAPIKey
	AuthSourceBearer   = api.AuthSourceBearer
	AuthSourceCombined = api.AuthSourceCombined
)

const (
	AuthMethodOAuth         = api.AuthMethodOAuth
	AuthMethodAPIKey        = api.AuthMethodAPIKey
	AuthMethodIAM           = api.AuthMethodIAM
	AuthMethodADC           = api.AuthMethodADC
	AuthMethodAzureIdentity = api.AuthMethodAzureIdentity
)

// NoAuth returns an AuthSource with no credentials.
var NoAuth = api.NoAuth

// APIKeyAuth returns an AuthSource using an API key.
var APIKeyAuth = api.APIKeyAuth

// BearerAuth returns an AuthSource using a bearer token.
var BearerAuth = api.BearerAuth

// CombinedAuth returns an AuthSource using both API key and bearer token.
var CombinedAuth = api.CombinedAuth

// ResolveStartupAuth resolves auth from environment variables.
var ResolveStartupAuth = api.ResolveStartupAuth

// EnrichBearerAuthError adds a helpful hint for sk-ant-* bearer token misuse.
var EnrichBearerAuthError = api.EnrichBearerAuthError
