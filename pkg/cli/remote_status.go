package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/server"
)

// RemoteServerBuild IS the build block of the server's GET /api/server/info
// answer, aliased: the decode target carries the server's own tags, and
// only the version and commit, so the probe reads them whatever else that
// answer carries.
type RemoteServerBuild = server.ServerBuild

// RemoteStatus is what `iterion remote status --json` prints and the operator
// MCP's remote_status answers: the instance, the account with its orgs and
// teams, the client's own version, and what the instance runs.
//
// Server is absent when the instance did not tell its version; then
// ServerInfoError says why (an unreachable route, an error status, an answer
// that is not the server's info, a timeout). The status itself still
// succeeds: the version is an addition to the account check, not a
// condition of it.
type RemoteStatus struct {
	Instance        string               `json:"instance"`
	Email           string               `json:"email"`
	Name            string               `json:"name"`
	IsSuperAdmin    bool                 `json:"is_super_admin"`
	ActiveOrgID     string               `json:"active_org_id"`
	ActiveTeamID    string               `json:"active_team_id"`
	Orgs            []server.OrgTreeView `json:"orgs"`
	ClientVersion   string               `json:"client_version"`
	Server          *RemoteServerBuild   `json:"server,omitempty"`
	ServerInfoError string               `json:"server_info_error,omitempty"`
}

// serverInfoTimeoutEnv raises (or lowers) the bound of the version probe.
const serverInfoTimeoutEnv = "ITERION_REMOTE_SERVER_INFO_TIMEOUT"

// defaultServerInfoTimeout bounds the version probe, so an instance whose
// info route hangs leaves the status without a version instead of hanging
// it for the client's whole request timeout.
const defaultServerInfoTimeout = 5 * time.Second

// serverInfoTimeout is the version probe's bound: the default, or the
// operator's ITERION_REMOTE_SERVER_INFO_TIMEOUT (a positive Go duration).
func serverInfoTimeout() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(serverInfoTimeoutEnv))
	if v == "" {
		return defaultServerInfoTimeout, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q is not a positive Go duration (e.g. 10s)", serverInfoTimeoutEnv, v)
	}
	return d, nil
}

// ServerBuild reads what the instance runs from GET /api/server/info, a
// public route, within the probe's bound. A version-less answer is an
// error: a server older than the route answers 404, a non-iterion host or
// a dispatcher's loopback mux answers without one.
func (c *RemoteClient) ServerBuild(ctx context.Context) (RemoteServerBuild, error) {
	const path = "/api/server/info"
	var build RemoteServerBuild
	timeout, err := serverInfoTimeout()
	if err != nil {
		return build, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := c.Call(probeCtx, "GET", path, nil, &build); err != nil {
		if ctx.Err() == nil && errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return RemoteServerBuild{}, fmt.Errorf("GET %s: no answer within %s (%s raises it)", path, timeout, serverInfoTimeoutEnv)
		}
		return RemoteServerBuild{}, err
	}
	if strings.TrimSpace(build.Version) == "" {
		return RemoteServerBuild{}, fmt.Errorf("GET %s answered no version", path)
	}
	return build, nil
}

// Status reads the account (GET /api/auth/me) and what the instance runs
// (ServerBuild). Only the account read fails it.
func (c *RemoteClient) Status(ctx context.Context) (RemoteStatus, error) {
	me, err := c.Me(ctx)
	if err != nil {
		return RemoteStatus{}, err
	}
	st := RemoteStatus{
		Instance:      c.BaseURL(),
		Email:         me.User.Email,
		Name:          me.User.Name,
		IsSuperAdmin:  me.User.IsSuperAdmin,
		ActiveOrgID:   me.ActiveOrg,
		ActiveTeamID:  me.ActiveTeam,
		Orgs:          me.Orgs,
		ClientVersion: RawVersion(),
	}
	build, err := c.ServerBuild(ctx)
	if err != nil {
		st.ServerInfoError = err.Error()
	} else {
		st.Server = &build
	}
	return st, nil
}

// RemoteStatusPrint renders Status: the whole answer in JSON mode, the
// instance, the account and both versions for a terminal.
func RemoteStatusPrint(ctx context.Context, c *RemoteClient, p *Printer) error {
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(st)
		return nil
	}
	role := ""
	if st.IsSuperAdmin {
		role = " (super-admin)"
	}
	p.Line("Instance: %s", st.Instance)
	p.Line("Account:  %s%s", st.Email, role)
	switch {
	case st.Server == nil:
		p.Line("Server:   version unknown (%s)", st.ServerInfoError)
	case st.Server.Commit != "":
		p.Line("Server:   %s (commit %s)", st.Server.Version, st.Server.Commit)
	default:
		p.Line("Server:   %s", st.Server.Version)
	}
	p.Line("Client:   %s", st.ClientVersion)
	return nil
}
