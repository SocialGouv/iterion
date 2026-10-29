// Only GOOS=windows compiles this file, and nothing but its filename says
// so: the crossing it carries is what a guard listing linux/amd64 misses.
package store

import "example.com/crossbuild/pkg/cli"

// Lock crosses from the leaf into the CLI layer.
func Lock() string { return cli.Name() }
