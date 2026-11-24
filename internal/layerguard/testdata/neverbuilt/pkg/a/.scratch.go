// The FILE half of the convention, dot side — go/build drops this name in
// every context too, and gofmt's own directory walk skips it, which is why
// the repository can carry it as the fixture it is.
package a

import "example.com/neverbuilt/internal/b"

// DotScratch is never compiled as part of package a.
func DotScratch() string { return b.Name() }
