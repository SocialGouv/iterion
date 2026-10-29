// The dot half of the same convention: a pattern names no package in a
// directory whose name starts with . either, and this file carries no build
// constraint. Package a imports it too, so both halves reach the listing
// through an edge and neither reaches it through a pattern.
package hidden

// Name answers something, so the package has a body.
func Name() string { return "hidden" }
