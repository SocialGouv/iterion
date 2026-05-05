//go:build !desktop

package main

import (
	"fmt"
	"os"
)

// main_stub is the entrypoint that executes when iterion-desktop is built
// without the `desktop` build tag. It exists purely so `go build ./...`
// and `go test ./...` keep working without Wails being installed as a
// Go module dependency.
//
// To produce the real desktop binary, build with:
//
//	wails build -tags desktop
//	# or
//	go build -tags desktop ./cmd/iterion-desktop
//
// (the Wails CLI invokes the latter under the hood with its own flags).
func main() {
	fmt.Fprintln(os.Stderr, "iterion-desktop must be built with `-tags desktop` (Wails). See docs/desktop-build.md.")
	os.Exit(1)
}
