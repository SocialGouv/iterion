// The dot half of the same convention: `go list ./pkg/...` skips a directory
// whose name starts with . too, and this file carries no build constraint
// either.
package hidden

// Name answers something, so the package has a body.
func Name() string { return "hidden" }
