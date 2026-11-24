package model

import "github.com/SocialGouv/iterion/pkg/backend/llmtypes"

// ModelCapabilities describes what features a model supports.
// Re-exported from llmtypes to keep the model package API stable.
type ModelCapabilities = llmtypes.ModelCapabilities

// Known is a capability flag a table answered (llmtypes.Known).
func Known(v bool) *bool { return llmtypes.Known(v) }
