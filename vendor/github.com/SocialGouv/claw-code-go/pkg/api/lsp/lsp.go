// Package lsp is the public façade over the internal LSP subsystem
// that backs the `lsp` tool.
package lsp

import (
	lsppkg "github.com/SocialGouv/claw-code-go/internal/lsp"
)

type Registry = lsppkg.Registry

func NewRegistry() *Registry { return lsppkg.NewRegistry() }
