package mcp

import "testing"

func TestAuthState_DefaultDisconnected(t *testing.T) {
	a := NewAuthState()
	if s := a.GetStatus("unknown-server"); s != McpDisconnected {
		t.Errorf("expected McpDisconnected for unknown server, got %s", s)
	}
}

func TestAuthState_SetAndGet(t *testing.T) {
	a := NewAuthState()
	a.SetStatus("srv1", McpConnected)
	if s := a.GetStatus("srv1"); s != McpConnected {
		t.Errorf("expected McpConnected, got %s", s)
	}
	a.SetStatus("srv1", McpAuthRequired)
	if s := a.GetStatus("srv1"); s != McpAuthRequired {
		t.Errorf("expected McpAuthRequired, got %s", s)
	}
}

func TestAuthState_AllStatuses(t *testing.T) {
	a := NewAuthState()
	a.SetStatus("srv1", McpConnected)
	a.SetStatus("srv2", McpError)

	all := a.AllStatuses()
	if len(all) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(all))
	}
	if all["srv1"] != McpConnected {
		t.Errorf("srv1: expected connected, got %s", all["srv1"])
	}
	if all["srv2"] != McpError {
		t.Errorf("srv2: expected error, got %s", all["srv2"])
	}
}

func TestMcpConnectionStatus_String(t *testing.T) {
	tests := []struct {
		status McpConnectionStatus
		want   string
	}{
		{McpDisconnected, "disconnected"},
		{McpConnecting, "connecting"},
		{McpConnected, "connected"},
		{McpAuthRequired, "auth_required"},
		{McpError, "error"},
		{McpConnectionStatus(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.status.String(); got != tt.want {
			t.Errorf("McpConnectionStatus(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}
