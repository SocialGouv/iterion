package mcp

import "sync"

// McpConnectionStatus represents the connection state of an MCP server.
type McpConnectionStatus int

const (
	McpDisconnected McpConnectionStatus = iota
	McpConnecting
	McpConnected
	McpAuthRequired
	McpError
)

var mcpStatusStrings = [...]string{
	"disconnected",
	"connecting",
	"connected",
	"auth_required",
	"error",
}

func (s McpConnectionStatus) String() string {
	if int(s) < len(mcpStatusStrings) {
		return mcpStatusStrings[s]
	}
	return "unknown"
}

// AuthState tracks per-server authentication and connection status.
type AuthState struct {
	mu       sync.RWMutex
	statuses map[string]McpConnectionStatus
}

// NewAuthState creates a new AuthState tracker.
func NewAuthState() *AuthState {
	return &AuthState{
		statuses: make(map[string]McpConnectionStatus),
	}
}

// SetStatus sets the connection status for a server.
func (a *AuthState) SetStatus(serverName string, status McpConnectionStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statuses[serverName] = status
}

// GetStatus returns the connection status for a server.
func (a *AuthState) GetStatus(serverName string) McpConnectionStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if s, ok := a.statuses[serverName]; ok {
		return s
	}
	return McpDisconnected
}

// AllStatuses returns a snapshot of all server statuses.
func (a *AuthState) AllStatuses() map[string]McpConnectionStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make(map[string]McpConnectionStatus, len(a.statuses))
	for k, v := range a.statuses {
		result[k] = v
	}
	return result
}
