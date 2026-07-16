package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// exampleCancellation demonstrates cancelling a long-running callback.
func exampleCancellation() {
	fmt.Println("=== Cancellation Example ===")
	fmt.Println("This example demonstrates cancellation of a long-running permission callback.")
	fmt.Println("The callback is driven directly so the cancellation behavior is deterministic.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	callbackStarted := make(chan struct{})

	var callbackStartedOnce sync.Once

	longRunningCallback := func(
		ctx context.Context,
		toolName string,
		_ map[string]any,
		_ *codexsdk.ToolPermissionContext,
	) (codexsdk.PermissionResult, error) {
		if toolName != "Bash" && toolName != "Write" && toolName != "Edit" {
			return &codexsdk.PermissionResultAllow{}, nil
		}

		fmt.Printf("[CALLBACK] Starting long-running check for tool: %s\n", toolName)
		callbackStartedOnce.Do(func() { close(callbackStarted) })
		fmt.Println("[CALLBACK] Simulating work until context cancellation")

		for i := 1; i <= 10; i++ {
			select {
			case <-ctx.Done():
				fmt.Printf("[CALLBACK] Operation cancelled after %d seconds!\n", i-1)
				fmt.Printf("[CALLBACK] Cancellation reason: %v\n", ctx.Err())

				return nil, ctx.Err()
			case <-time.After(1 * time.Second):
				fmt.Printf("[CALLBACK] Working... %d/10 seconds\n", i)
			}
		}

		fmt.Println("[CALLBACK] Operation completed successfully")

		return &codexsdk.PermissionResultAllow{}, nil
	}

	fmt.Println("[MAIN] Starting simulated permission callback...")
	fmt.Println()

	queryDone := make(chan error, 1)

	go func() {
		_, err := longRunningCallback(
			ctx,
			"Bash",
			map[string]any{"command": "printf 'Hello World' > cancellation_demo.txt"},
			&codexsdk.ToolPermissionContext{},
		)
		queryDone <- err
	}()

	select {
	case <-callbackStarted:
		fmt.Println("[MAIN] Callback started; cancelling context in 2 seconds...")
		time.Sleep(2 * time.Second)
		cancel()
	case <-time.After(30 * time.Second):
		fmt.Println("[MAIN] Timeout waiting for callback to start")

		return
	}

	select {
	case err := <-queryDone:
		if err != nil {
			fmt.Printf("[MAIN] Callback ended with error (expected after cancel): %v\n", err)
		}
	case <-time.After(15 * time.Second):
		fmt.Println("[MAIN] Callback did not finish in time after cancellation")
	}

	fmt.Println("[MAIN] Cancellation example completed")
	fmt.Println()
}

// exampleGracefulShutdown demonstrates graceful shutdown with in-flight callbacks.
func exampleGracefulShutdown() {
	fmt.Println("=== Graceful Shutdown Example ===")
	fmt.Println("This example demonstrates shutdown of an in-flight permission callback.")
	fmt.Println("The callback is driven directly so the shutdown behavior is deterministic.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	callbackStarted := make(chan struct{})
	callbackDone := make(chan struct{})

	var callbackStartedOnce sync.Once

	waitingCallback := func(
		ctx context.Context,
		toolName string,
		_ map[string]any,
		_ *codexsdk.ToolPermissionContext,
	) (codexsdk.PermissionResult, error) {
		if toolName != "Bash" && toolName != "Write" && toolName != "Edit" {
			return &codexsdk.PermissionResultAllow{}, nil
		}

		fmt.Printf("[CALLBACK] Started for tool: %s\n", toolName)
		callbackStartedOnce.Do(func() { close(callbackStarted) })

		<-ctx.Done()
		fmt.Println("[CALLBACK] Context cancelled during graceful shutdown")
		close(callbackDone)

		return nil, ctx.Err()
	}

	go func() {
		_, err := waitingCallback(
			ctx,
			"Bash",
			map[string]any{"command": "printf 'test' > graceful_shutdown_demo.txt"},
			&codexsdk.ToolPermissionContext{},
		)
		if err != nil {
			fmt.Printf("Query error (expected during shutdown): %v\n", err)
		}
	}()

	select {
	case <-callbackStarted:
		fmt.Println("[MAIN] Callback is running, initiating graceful shutdown...")
	case <-time.After(30 * time.Second):
		fmt.Println("[MAIN] Timeout waiting for callback to start")

		return
	}

	time.Sleep(500 * time.Millisecond)

	fmt.Println("[MAIN] Simulating client shutdown by cancelling the callback context")
	cancel()
	fmt.Println("[MAIN] Shutdown signal sent")

	select {
	case <-callbackDone:
		fmt.Println("[MAIN] In-flight callback exited after shutdown")
	case <-time.After(10 * time.Second):
		fmt.Println("[MAIN] Timeout waiting for callback to exit")
	}

	fmt.Println("[MAIN] Graceful shutdown example completed")
	fmt.Println()
}

func main() {
	fmt.Println("Starting Codex SDK Cancellation Examples...")
	fmt.Println("============================================")
	fmt.Println()

	examples := map[string]func(){
		"cancellation":      exampleCancellation,
		"graceful_shutdown": exampleGracefulShutdown,
	}

	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <example_name>")
		fmt.Println("\nAvailable examples:")
		fmt.Println("  all               - Run all examples")
		fmt.Println("  cancellation      - Demonstrate cancelling a long-running callback")
		fmt.Println("  graceful_shutdown - Demonstrate graceful shutdown")

		return
	}

	exampleName := os.Args[1]

	if exampleName == "all" {
		exampleOrder := []string{"cancellation", "graceful_shutdown"}
		for _, name := range exampleOrder {
			if fn, ok := examples[name]; ok {
				fn()
				fmt.Println("--------------------------------------------------")
				fmt.Println()
			}
		}
	} else if fn, ok := examples[exampleName]; ok {
		fn()
	} else {
		fmt.Printf("Unknown example: %s\n", exampleName)
		fmt.Println("\nAvailable examples:")
		fmt.Println("  all               - Run all examples")

		for name := range examples {
			fmt.Printf("  %s\n", name)
		}

		os.Exit(1)
	}
}
