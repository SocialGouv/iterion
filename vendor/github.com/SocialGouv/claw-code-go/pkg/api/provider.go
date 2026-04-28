package api

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

// ProviderConfig holds the credentials and settings needed to create a provider client.
type ProviderConfig = api.ProviderConfig

// APIClient is the interface all provider clients must implement.
type APIClient = api.APIClient

// Provider is the interface all AI providers must implement.
type Provider = api.Provider
