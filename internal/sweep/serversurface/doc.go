// Package serversurface guards the seams between the server's REST surface
// and the sources that spell it from outside pkg/server: every /api path and
// (method, path) pair the CLI (cmd/iterion, pkg/cli) sends is routed, the
// declared concatenation bases are still bases, every route the curated REST
// reference (docs/cloud-rest-api.md) documents is served, and the cloud
// bootstrap (cmd/iterion/server.go) wires the maintenance fence.
//
// The served table is derived from the registrars' source by
// internal/routescan, the scanner pkg/server's
// TestTheSourceDerivedTableCoversTheRunningOne certifies against a running
// server. These guards live outside pkg/server because they read the CLI,
// the dispatcher's registrars and the docs: hosted there, a change to any of
// them would select pkg/server's own behaviour tests.
package serversurface
