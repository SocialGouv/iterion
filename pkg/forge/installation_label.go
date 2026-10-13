package forge

import (
	"context"
	"net/http"
)

// InstallationClient returns a client that sends through c, each request
// labelled with the GitHub App installation whose token authenticates it, so
// a transport below can account the request to that installation's budget
// (InstallationOf). It shares c's transport — connection pool included — and
// c's timeout, redirect policy and cookie jar; c itself is left unchanged. An
// id of 0 names no installation, and c is returned as it is.
func InstallationClient(c *http.Client, installationID int64) *http.Client {
	if installationID == 0 {
		return c
	}
	if c == nil {
		c = http.DefaultClient
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	labelled := *c
	labelled.Transport = installationTransport{base: base, installation: installationID}
	return &labelled
}

type installationKey struct{}

// installationTransport labels every request it sends. The client sends each
// redirect hop through its transport, so a hop carries the label too.
type installationTransport struct {
	base         http.RoundTripper
	installation int64
}

func (t installationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(context.WithValue(req.Context(), installationKey{}, t.installation)))
}

// InstallationOf reads the installation InstallationClient labelled a
// request's context with. ok is false for a request sent with any other
// credential: a PAT, a user token, the App's own JWT.
func InstallationOf(ctx context.Context) (installationID int64, ok bool) {
	installationID, ok = ctx.Value(installationKey{}).(int64)
	return installationID, ok
}
