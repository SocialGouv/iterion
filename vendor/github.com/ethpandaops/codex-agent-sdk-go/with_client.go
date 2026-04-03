package codexsdk

import (
	"context"
	"fmt"
)

// WithClient manages client lifecycle with automatic cleanup.
//
// This helper creates a client, starts it with the provided options, executes the
// callback function, and ensures proper cleanup via Close() when done.
//
// The callback receives a fully initialized Client that is ready for use.
// If the callback returns an error, it is returned to the caller.
// If Close() fails, a warning is logged but does not override the callback's error.
//
// Example usage:
//
//	err := codexsdk.WithClient(ctx, func(c codexsdk.Client) error {
//	    if err := c.Query(ctx, Text("Hello")); err != nil {
//	        return err
//	    }
//	    for msg, err := range c.ReceiveResponse(ctx) {
//	        if err != nil {
//	            return err
//	        }
//	        // process message...
//	    }
//	    return nil
//	},
//	    codexsdk.WithLogger(log),
//	    codexsdk.WithPermissionMode("acceptEdits"),
//	)
func WithClient(ctx context.Context, fn func(Client) error, opts ...Option) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	options := applyAgentOptions(opts)

	log := options.Logger
	if log == nil {
		log = NopLogger()
	}

	client := NewClient()
	if err := client.Start(ctx, opts...); err != nil {
		return fmt.Errorf("failed to start client: %w", err)
	}

	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			log.Warn("failed to close client", "error", closeErr)
		}
	}()

	return fn(client)
}
