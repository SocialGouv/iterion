//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

// Both values are immutable image-build inputs, overridable only by the linker
// when building a compatible custom image pair, never by runtime environment.
var capsulePath = "/usr/local/libexec/iterion-execenv-capsule"
var payloadPath = "/usr/local/libexec/iterion-execenv-guest"

func main() {
	code, err := protocol.RunGuest(protocol.GuestPaths{Capsule: capsulePath, Payload: payloadPath})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
