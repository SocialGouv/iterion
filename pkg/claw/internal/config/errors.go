package config

import "fmt"

// ConfigErrorKind classifies the category of a config error.
type ConfigErrorKind int

const (
	// ConfigErrIO indicates a filesystem I/O error.
	ConfigErrIO ConfigErrorKind = iota
	// ConfigErrJSON indicates a JSON serialization/deserialization error.
	ConfigErrJSON
	// ConfigErrInvalidConfig indicates the configuration is structurally invalid.
	ConfigErrInvalidConfig
	// ConfigErrUnknownMcpServer indicates a reference to an undefined MCP server.
	ConfigErrUnknownMcpServer
	// ConfigErrMcpConfigConflict indicates conflicting MCP server configurations.
	ConfigErrMcpConfigConflict
)

func (k ConfigErrorKind) String() string {
	switch k {
	case ConfigErrIO:
		return "io"
	case ConfigErrJSON:
		return "json"
	case ConfigErrInvalidConfig:
		return "invalid_config"
	case ConfigErrUnknownMcpServer:
		return "unknown_mcp_server"
	case ConfigErrMcpConfigConflict:
		return "mcp_config_conflict"
	default:
		return "unknown"
	}
}

// ConfigError is a structured error for config operations.
type ConfigError struct {
	Kind    ConfigErrorKind
	Op      string
	Path    string
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *ConfigError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("config %s [%s]: %s", e.Op, e.Path, e.Message)
	}
	return fmt.Sprintf("config %s: %s", e.Op, e.Message)
}

// Unwrap returns the underlying cause, enabling errors.Is and errors.As.
func (e *ConfigError) Unwrap() error {
	return e.Cause
}

// NewConfigIOError creates a ConfigError for I/O failures.
func NewConfigIOError(op, path string, cause error) *ConfigError {
	msg := "i/o error"
	if cause != nil {
		msg = cause.Error()
	}
	return &ConfigError{
		Kind:    ConfigErrIO,
		Op:      op,
		Path:    path,
		Message: msg,
		Cause:   cause,
	}
}

// NewConfigJSONError creates a ConfigError for JSON failures.
func NewConfigJSONError(op string, cause error) *ConfigError {
	msg := "json error"
	if cause != nil {
		msg = cause.Error()
	}
	return &ConfigError{
		Kind:    ConfigErrJSON,
		Op:      op,
		Message: msg,
		Cause:   cause,
	}
}

// NewConfigError creates a ConfigError with the given kind and message.
func NewConfigError(kind ConfigErrorKind, msg string) *ConfigError {
	return &ConfigError{
		Kind:    kind,
		Op:      "validate",
		Message: msg,
	}
}
