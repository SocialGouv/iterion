// Package mintchokepoint guards the derivation chokepoint over team-token
// mints across the whole module: every mint primitive demands the gate's
// verdict, the verdict is stamped only by its printer and by
// pkg/server/derivation_gate.go, the forge asks the chokepoint before its one
// mint, and every caller of a primitive lives in the directories these guards
// read.
//
// It lives outside pkg/server because it parses every non-test .go file of
// the module: hosted there, any Go change would select pkg/server's own
// behaviour tests. The guards judge files by their module-relative path, so
// moving them changes nothing they see. The checks that read pkg/server
// alone — the forge orchestrator built with its mint seam, and the OpenAPI
// table of gated routes — stay in pkg/server.
package mintchokepoint
