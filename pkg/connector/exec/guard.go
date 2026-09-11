package exec

import "net/http"

// The mark that says an HTTP client is fit to carry a connector call.
//
// A connector reaches a host a TENANT chose, so the client it goes out on has
// to be the address-checked, redirect-refusing one. Nothing about an
// *http.Client says that from the outside: a default client and a guarded one
// have the same type, so until a call is made they are indistinguishable. The
// posture therefore rested on every injection site remembering to pass the
// right one — a convention, held today by there being exactly one production
// site, and due to break at the second.
//
// The mark moves the property onto the VALUE. A client built for connector
// calls carries it; Call refuses one that does not. A new wiring that forgets
// fails loudly on its first call instead of quietly reaching the deployment's
// own network.

// guardedTransport carries the mark. It adds no behaviour: the guarantees are
// the wrapped client's, and wrapping is only how a fact about it becomes
// readable.
type guardedTransport struct{ base http.RoundTripper }

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req)
}

// Unwrap keeps the mark reachable when a later wrapper sits on top of it.
func (t *guardedTransport) Unwrap() http.RoundTripper { return t.base }

// MarkGuarded returns a client carrying the mark, leaving c untouched.
//
// The caller ASSERTS the property — a dialer cannot be verified from the
// outside, so this is a declaration, not a proof. What it buys is that the
// declaration is made ONCE, where the client is built and where the guarantees
// are visible, instead of being re-assumed at each site that hands a client on.
//
// A COPY rather than a mutation, because a caller is free to pass a client it
// does not own: marking http.DefaultClient in place would put the mark on every
// unrelated request in the process, which is the opposite of a rule that exists
// to keep an unguarded call from happening. The copy shares the underlying
// transport, so connection pooling is unaffected.
//
// Production has one caller (connection.LocalHTTPClient). A test that injects a
// stub server's client is the other, and the name is what makes that visible in
// a diff.
func MarkGuarded(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	base := c.Transport
	if base == nil {
		// A nil Transport means http.DefaultTransport, which is precisely the
		// unguarded one. Marking it is still the caller's call to make — a
		// stub server's client is the ordinary case — but the indirection is
		// resolved here so IsGuarded never has to interpret nil.
		base = http.DefaultTransport
	}
	marked := *c
	marked.Transport = &guardedTransport{base: base}
	return &marked
}

// maxTransportDepth bounds the Unwrap walk.
//
// The walk follows a chain whose links this package does not own, so its
// termination would otherwise depend on every wrapper being well behaved: one
// whose Unwrap returns itself spins forever, and it would spin inside Call, on
// every connector request, holding the run's lease with nothing to show for it.
// A real chain is two or three deep — the guarded transport, a diagnostic
// layer, perhaps a recorder — so the bound costs nothing a legitimate caller
// can feel.
const maxTransportDepth = 16

// IsGuarded reports whether c carries the mark.
//
// It walks the Unwrap chain so a transport wrapper added after the mark (a
// diagnostic one, say) does not hide it. Anything the walk cannot follow to the
// mark — a wrapper that forwards neither the type nor Unwrap, a chain deeper
// than maxTransportDepth, a cycle — makes this return false, which costs a
// REFUSED call rather than an unguarded one. That is the direction to fail in:
// a client wrongly refused is a loud wiring bug, a client wrongly accepted is a
// tenant reaching the deployment's own network.
func IsGuarded(c *http.Client) bool {
	if c == nil {
		return false
	}
	rt := c.Transport
	for depth := 0; rt != nil && depth < maxTransportDepth; depth++ {
		if _, ok := rt.(*guardedTransport); ok {
			return true
		}
		u, ok := rt.(interface{ Unwrap() http.RoundTripper })
		if !ok {
			return false
		}
		rt = u.Unwrap()
	}
	return false
}
