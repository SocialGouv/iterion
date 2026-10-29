// Package connectors exposes the connector packages shipped with iterion as
// an fs.FS, so the binary serves them from any working directory — the BAKED
// floor of the three-tier distribution (baked / platform / team).
//
// Why a floor at all: a package is `connectors/<id>/`, a directory of YAML,
// and until it travelled in the binary the only processes that could resolve
// one were those standing in a checkout of this repository. A published
// `iterion` served no connector at all, and `.bot` files calling
// `forgejo.issue.comment` failed with "no connector catalog" on every machine
// that had installed the binary rather than cloned the tree.
//
// What travels is the WHOLE package directory (`all:` so nothing is skipped
// for its name): the generated half, its authored overlay, the schemas, the
// response contracts, the triggers and the identity lock. Not a curated
// subset — the overlay is what pins the operation ids a `.bot` addresses and
// replaces the auth the vendor's description gets wrong, so a package served
// without it answers to different ids with a different credential placement.
// This is the same bargain bots/embed.go makes for the productised recipes,
// and the opposite choice from the bundles it deliberately leaves out: a
// connector package has no adjacent resources to lose, and it is read in
// full by one loader (pkg/connector/overlay.LoadPackageFS).
//
// It is a package in a directory that is otherwise not Go — the form
// bots/embed.go already established — because //go:embed only reaches files
// in its own directory's tree.
package connectors

import (
	"embed"
	"io/fs"
)

// The shipped packages, embedded whole. A connector added under connectors/
// has to be named here too, which connectors.TestEveryShippedPackageIsBaked
// refuses to let slide: an unbaked package is one a published binary cannot
// resolve, with nothing on the resolution path saying why.
//
//go:embed all:forgejo
var files embed.FS

// FS is the baked packages, rooted so that a connector id is a directory in
// it — the shape pkg/connection's catalog reads, and the same shape
// `<iterion home>/connectors` has on disk.
//
// An fs.FS rather than the embed.FS itself: the only thing anybody may do
// with the baked packages is READ them through the one package loader. A
// caller holding the embed.FS would be free to grow a second reader of the
// bytes, which is the drift this seam exists to prevent.
func FS() fs.FS { return files }
