// Package lsp is the public façade over the internal LSP subsystem
// that backs the `lsp` tool.
package lsp

import (
	lsppkg "github.com/SocialGouv/claw-code-go/internal/lsp"
)

type (
	Registry        = lsppkg.Registry
	LspServerStatus = lsppkg.LspServerStatus
)

const (
	StatusConnected    = lsppkg.StatusConnected
	StatusDisconnected = lsppkg.StatusDisconnected
	StatusStarting     = lsppkg.StatusStarting
	StatusError        = lsppkg.StatusError
)

func NewRegistry() *Registry { return lsppkg.NewRegistry() }
