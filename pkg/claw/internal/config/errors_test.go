package config

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestConfigError_Kind(t *testing.T) {
	tests := []struct {
		name string
		err  *ConfigError
		want ConfigErrorKind
	}{
		{
			name: "IO error",
			err:  NewConfigIOError("read", "/etc/config.json", os.ErrNotExist),
			want: ConfigErrIO,
		},
		{
			name: "JSON error",
			err:  NewConfigJSONError("parse", fmt.Errorf("invalid character")),
			want: ConfigErrJSON,
		},
		{
			name: "InvalidConfig error",
			err:  NewConfigError(ConfigErrInvalidConfig, "missing required field"),
			want: ConfigErrInvalidConfig,
		},
		{
			name: "UnknownMcpServer error",
			err:  NewConfigError(ConfigErrUnknownMcpServer, "server 'foo' not defined"),
			want: ConfigErrUnknownMcpServer,
		},
		{
			name: "McpConfigConflict error",
			err:  NewConfigError(ConfigErrMcpConfigConflict, "duplicate server definition"),
			want: ConfigErrMcpConfigConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", tt.err.Kind, tt.want)
			}
		})
	}
}

func TestConfigError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *ConfigError
		want string
	}{
		{
			name: "with path",
			err:  NewConfigIOError("read", "/etc/config.json", os.ErrNotExist),
			want: "config read [/etc/config.json]: file does not exist",
		},
		{
			name: "without path",
			err:  NewConfigJSONError("parse", fmt.Errorf("invalid character")),
			want: "config parse: invalid character",
		},
		{
			name: "validate error",
			err:  NewConfigError(ConfigErrInvalidConfig, "missing required field"),
			want: "config validate: missing required field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigError_Unwrap(t *testing.T) {
	cause := os.ErrPermission
	err := NewConfigIOError("read", "/etc/config.json", cause)

	if !errors.Is(err, os.ErrPermission) {
		t.Error("errors.Is should find os.ErrPermission in chain")
	}

	// Validate error has no cause
	valErr := NewConfigError(ConfigErrInvalidConfig, "bad config")
	if valErr.Unwrap() != nil {
		t.Error("validate error Unwrap should return nil")
	}
}

func TestConfigError_ErrorsAs(t *testing.T) {
	cause := fmt.Errorf("permission denied")
	err := NewConfigIOError("read", "/etc/config.json", cause)

	// Wrap it further
	wrapped := fmt.Errorf("loading config: %w", err)

	var cfgErr *ConfigError
	if !errors.As(wrapped, &cfgErr) {
		t.Fatal("errors.As should extract *ConfigError from wrapped error")
	}
	if cfgErr.Kind != ConfigErrIO {
		t.Errorf("Kind = %v, want %v", cfgErr.Kind, ConfigErrIO)
	}
	if cfgErr.Path != "/etc/config.json" {
		t.Errorf("Path = %q, want %q", cfgErr.Path, "/etc/config.json")
	}
}
