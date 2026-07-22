package forge

import "context"

// PermissionClient is the optional per-user repo-permission capability a
// provider's admin client may expose, type-asserted like IssueClient
// (`if pc, ok := admin.(PermissionClient)`). It powers the author-trust
// gate: classifying whether the login behind an inbound issue holds real
// rights on the repo before any bot run is spent on it.
//
// CollaboratorPermission returns the user's effective permission on repo in
// the GitHub role vocabulary — admin|maintain|write|triage|read|none — which
// the server ranks against a gitlab-vocab minimum role (the same cross-forge
// scale as the webhook command gate). "Not a member" is ("none", nil), never
// an error; an error means the answer is unknown (callers must fail closed).
type PermissionClient interface {
	CollaboratorPermission(ctx context.Context, repo, user string) (string, error)
}
