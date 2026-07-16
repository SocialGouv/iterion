package codexsdk

import "github.com/ethpandaops/codex-agent-sdk-go/internal/config"

// Transport defines the interface for Codex CLI communication.
// Implement this to provide custom transports for testing, mocking,
// or alternative communication methods (e.g., remote connections).
//
// The default implementation is CLITransport which spawns a subprocess.
// Custom transports can be injected via CodexAgentOptions.Transport.
type Transport = config.Transport
