package api

import (
	"claw-code-go/internal/api"
)

// Client is the Anthropic HTTP API client.
type Client = api.Client

// NewClient creates a new API client with the given API key and model.
func NewClient(apiKey, model string) *Client {
	return api.NewClient(apiKey, model)
}
