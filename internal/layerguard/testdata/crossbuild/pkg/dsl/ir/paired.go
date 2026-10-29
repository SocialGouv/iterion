//go:build crossbuildonly && crossbuildpair

// Two project tags in ONE conjunction: a collector that reads one side of an
// && names the first and loses the second, and the tags context is then
// built without it — this file is what says so.
package ir

// Paired answers something, so the file has a body.
func Paired() string { return "paired" }
