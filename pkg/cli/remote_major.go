package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// RemoteClientHeader carries the CLI's own version (RawVersion) on every
// request the remote client sends, so an instance can tell which release
// talks to it and refuse a client too old to speak its API (HTTP 426).
const RemoteClientHeader = "X-Iterion-Client"

// serverInfoPath is the public route that answers what the instance runs
// (ServerBuild). The major gate reads it, so the gate never applies to it.
const serverInfoPath = "/api/server/info"

// allowMajorMismatchEnv lets a client talk to an instance of another major
// release: the major gate's escape hatch.
const allowMajorMismatchEnv = "ITERION_REMOTE_ALLOW_MAJOR_MISMATCH"

// clientTooOldCode is the error_code of a server's 426 answer that refuses
// the client's release.
const clientTooOldCode = "client_too_old"

// ErrMajorMismatch matches every refusal of the major gate, whichever side
// runs the newer release. ErrClientMajorOlder and ErrClientMajorNewer name
// the side.
var (
	ErrMajorMismatch    = errors.New("remote: client and server run different major releases")
	ErrClientMajorOlder = errors.New("remote: client runs an older major release than the server")
	ErrClientMajorNewer = errors.New("remote: client runs a newer major release than the server")
	// ErrClientTooOld matches a server's 426 client_too_old refusal.
	ErrClientTooOld = errors.New("remote: server refuses this client release as too old")
)

// MajorMismatchError is the major gate's refusal, raised before the request
// is sent: the client and the instance run releases of different majors.
type MajorMismatchError struct {
	Instance string
	Client   string
	Server   string
	// ClientOlder is true when the client's major is the lower one.
	ClientOlder bool
}

func (e *MajorMismatchError) Error() string {
	remedy := "upgrade the iterion CLI to the server's major"
	if !e.ClientOlder {
		remedy = "use an iterion CLI of the server's major"
	}
	return fmt.Sprintf("remote: client %s and server %s (%s) run different major releases: %s, or set %s=1 to send anyway",
		e.Client, e.Server, e.Instance, remedy, allowMajorMismatchEnv)
}

// Is matches ErrMajorMismatch and the sentinel of the older side.
func (e *MajorMismatchError) Is(target error) bool {
	switch target {
	case ErrMajorMismatch:
		return true
	case ErrClientMajorOlder:
		return e.ClientOlder
	case ErrClientMajorNewer:
		return !e.ClientOlder
	}
	return false
}

// ClientTooOldError is a server's HTTP 426 client_too_old refusal. Server is
// the version the instance answered when re-read after the refusal, empty
// when it told none. The server's answer unwraps as *APIError.
type ClientTooOldError struct {
	Client string
	Server string
	API    *APIError
}

func (e *ClientTooOldError) Error() string {
	server := e.Server
	if server == "" {
		server = "(version unknown)"
	}
	return fmt.Sprintf("remote: server %s refuses client %s as too old: upgrade the iterion CLI (%s)",
		server, e.Client, e.API.Error())
}

func (e *ClientTooOldError) Is(target error) bool { return target == ErrClientTooOld }

func (e *ClientTooOldError) Unwrap() error { return e.API }

// releaseMajor reads the major of an appinfo-style version ("v7.33.0",
// "7.33.0", "v8.0.0-rc.1", "v7"). ok is false for a version that names no
// release ("dev", "", "vdev", major 0): such a side is never gated. Major 0
// is the build placeholder (the Dockerfile's ARG VERSION=0.0.0), not a
// release.
func releaseMajor(v string) (int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if end < len(v) && !strings.ContainsRune(".-+", rune(v[end])) {
		return 0, false
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}

// allowMajorMismatch reads the gate's escape hatch. A value that is neither
// a yes nor a no is an error, so a typo never silently keeps the gate armed
// or disarmed.
func allowMajorMismatch() (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(allowMajorMismatchEnv))); v {
	case "":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q is neither a yes (1, true, yes, on) nor a no (0, false, no, off)", allowMajorMismatchEnv, v)
	}
}

// majorGate caches what the instance runs, read once per client from
// serverInfoPath and read again after a 404 or a 426 answer.
type majorGate struct {
	mu     sync.Mutex
	probed bool
	// server is the instance's version, empty when the probe read none
	// (a server older than the route, a version-less answer, no answer).
	server string
}

// invalidate makes the next gated request read the instance's version again.
func (g *majorGate) invalidate() {
	g.mu.Lock()
	g.probed = false
	g.server = ""
	g.mu.Unlock()
}

// seed records a version the client read itself (Status), so the next
// gated request does not read it again.
func (g *majorGate) seed(server string) {
	g.mu.Lock()
	g.probed = true
	g.server = server
	g.mu.Unlock()
}

// isServerInfoPath reports whether path (query included) is the probe route.
func isServerInfoPath(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	return p == serverInfoPath
}

// serverVersion returns the instance's version, probing it on the first call
// and after an invalidation. A failed probe is remembered as "unknown" so it
// is not repeated on every request, except when the caller's own context
// ended: that request fails anyway and says nothing about the server.
func (c *RemoteClient) serverVersion(ctx context.Context) (string, error) {
	g := &c.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probed {
		return g.server, nil
	}
	// A malformed probe bound is the operator's error to see, not a reason
	// to wave the request through ungated.
	if _, err := serverInfoTimeout(); err != nil {
		return "", err
	}
	build, err := c.ServerBuild(ctx)
	if err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	g.probed = true
	g.server = ""
	if err == nil {
		g.server = build.Version
	}
	return g.server, nil
}

// checkMajor refuses a request when the client and the instance both name a
// release and their majors differ. A side that names none — a "dev" client,
// a server older than serverInfoPath, a version-less or failed probe —
// passes: the gate refuses only what it knows.
func (c *RemoteClient) checkMajor(ctx context.Context) error {
	allow, err := allowMajorMismatch()
	if err != nil {
		return err
	}
	if allow {
		return nil
	}
	client := RawVersion()
	clientMajor, ok := releaseMajor(client)
	if !ok {
		return nil
	}
	server, err := c.serverVersion(ctx)
	if err != nil {
		return err
	}
	serverMajor, ok := releaseMajor(server)
	if !ok || serverMajor == clientMajor {
		return nil
	}
	return &MajorMismatchError{
		Instance:    c.BaseURL(),
		Client:      client,
		Server:      server,
		ClientOlder: clientMajor < serverMajor,
	}
}

// afterResponse updates the gate from an answer: a 404 may mean the instance
// behind the URL changed, so its version is read again on the next request;
// a 426 re-reads it at once and, when the server says client_too_old,
// becomes a *ClientTooOldError.
func (c *RemoteClient) afterResponse(ctx context.Context, method, path string, status int, body []byte) error {
	if isServerInfoPath(path) {
		return nil
	}
	switch status {
	case http.StatusNotFound:
		c.gate.invalidate()
	case http.StatusUpgradeRequired:
		c.gate.invalidate()
		server, err := c.serverVersion(ctx)
		if err != nil {
			return err
		}
		var eb errorBody
		if json.Unmarshal(body, &eb) == nil && eb.ErrorCode == clientTooOldCode {
			return &ClientTooOldError{
				Client: RawVersion(),
				Server: server,
				API:    &APIError{Status: status, Method: method, Path: path, Body: string(body)},
			}
		}
	}
	return nil
}
