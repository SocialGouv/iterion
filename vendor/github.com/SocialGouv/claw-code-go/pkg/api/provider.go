package api

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

// ProviderConfig holds the credentials and settings needed to create a provider client.
type ProviderConfig = api.ProviderConfig

// ChatGPTClientVersion is the codex-cli release claw presents on the
// ChatGPT-Codex wire when ProviderConfig.OpenAIClientVersion is empty; a
// caller probing a real Codex CLI should send the newer of the two.
const ChatGPTClientVersion = api.ChatGPTClientVersion

// APIClient is the interface all provider clients must implement.
type APIClient = api.APIClient

// Provider is the interface all AI providers must implement.
type Provider = api.Provider

// Client identity (see internal/api/identity.go for the contract): every
// provider sends an honest "claw-code-go/<version>" User-Agent by default,
// overridable via ProviderConfig.UserAgent, the CLAW_USER_AGENT environment
// variable, or ANTHROPIC_CUSTOM_HEADERS (Claude Code parity).
type Identity = api.Identity

var (
	DefaultUserAgent   = api.DefaultUserAgent
	ResolveIdentity    = api.ResolveIdentity
	ParseCustomHeaders = api.ParseCustomHeaders
)
