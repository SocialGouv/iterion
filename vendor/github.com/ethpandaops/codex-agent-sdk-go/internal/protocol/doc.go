// Package protocol implements bidirectional control message handling for the Codex CLI.
//
// The protocol package provides a Controller that manages request/response
// correlation for control messages sent to and received from the Codex CLI.
// This enables interactive features like interruption, dynamic configuration
// changes, and hook callbacks.
//
// The Controller handles:
//   - Sending control_request messages with unique IDs
//   - Receiving and correlating control_response messages
//   - Request timeout enforcement
//   - Handler registration for incoming requests from the CLI
//
// Example usage:
//
//	transport := subprocess.NewCLITransport(log, "", options)
//	transport.Start(ctx)
//
//	controller := protocol.NewController(log, transport)
//	controller.Start(ctx)
//
//	// Send a request with timeout
//	resp, err := controller.SendRequest(ctx, "interrupt", nil, 5*time.Second)
package protocol
