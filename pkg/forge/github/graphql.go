package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// GraphQL is GitHub's second API surface. It is not an alternative style for
// the same data: Projects v2 exists ONLY here, so a REST-only client cannot
// read a project board at all.
//
// The trap it carries — and the reason this transport is a shared helper
// rather than an inline call per query — is that a GraphQL failure is an HTTP
// **200** with a populated `data` object plus an `errors` array. A caller that
// decodes `data` and checks only the status code reads a zero value and
// reports success. Every call therefore goes through GraphQL below, which
// treats a non-empty `errors[]` as an error, whatever `data` says.

// GraphQLURLFor maps a GitHub WEB base URL to its GraphQL endpoint.
// github.com → api.github.com/graphql; a GitHub Enterprise host →
// <host>/api/graphql. Note the GHE path is /api/graphql, NOT /api/v3/graphql:
// v3 is the REST namespace and answers 404 for GraphQL.
func GraphQLURLFor(webBase string) string {
	b := strings.TrimRight(strings.TrimSpace(webBase), "/")
	switch b {
	case "", "https://github.com", "http://github.com":
		return "https://api.github.com/graphql"
	default:
		return b + "/api/graphql"
	}
}

// graphQLEndpoint derives this client's GraphQL URL from its REST API base, so
// a client built by New (which stores only APIBase) and one built as a struct
// literal in a test both reach the right host.
func (c *AdminClient) graphQLEndpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	switch {
	case base == "":
		return "https://api.github.com/graphql"
	case base == "https://api.github.com":
		return base + "/graphql"
	case strings.HasSuffix(base, "/api/v3"):
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	default:
		// A bare host (tests point APIBase at an httptest server).
		return base + "/graphql"
	}
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphQLError is one entry of a GraphQL response's errors array.
type graphQLError struct {
	Type    string   `json:"type,omitempty"`
	Message string   `json:"message"`
	Path    []any    `json:"path,omitempty"`
	Extra   struct{} `json:"-"`
}

func (e graphQLError) String() string {
	if e.Type == "" {
		return e.Message
	}
	return e.Type + ": " + e.Message
}

// GraphQLErrors is the typed error a GraphQL response's errors array becomes.
// It keeps the entries so a caller can map a known type (NOT_FOUND, FORBIDDEN)
// onto a forge sentinel instead of matching on message text.
type GraphQLErrors struct {
	Op     string
	Errors []graphQLError
}

func (g *GraphQLErrors) Error() string {
	parts := make([]string, 0, len(g.Errors))
	for _, e := range g.Errors {
		parts = append(parts, e.String())
	}
	return fmt.Sprintf("github: %s: graphql: %s", g.Op, strings.Join(parts, "; "))
}

// HasType reports whether any entry carries the given GraphQL error type.
func (g *GraphQLErrors) HasType(t string) bool {
	for _, e := range g.Errors {
		if strings.EqualFold(e.Type, t) {
			return true
		}
	}
	return false
}

// GraphQL performs one GraphQL call and decodes `data` into out.
//
// It fails on THREE independent conditions, and conflating any of them is how
// a board sync silently does nothing:
//   - transport / non-2xx status → the usual forge sentinel (401 → ErrUnauthorized);
//   - a non-empty `errors[]`, even alongside a populated `data` → *GraphQLErrors;
//   - a malformed body.
//
// out may be nil for a mutation whose result is not needed.
func (c *AdminClient) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	return c.graphQLOp(ctx, "graphql", query, vars, out)
}

// graphQLOp is GraphQL with a caller-supplied operation label for errors.
func (c *AdminClient) graphQLOp(ctx context.Context, op, query string, vars map[string]any, out any) error {
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	code, err := forge.DoJSON(ctx, c.HTTP, http.MethodPost, c.graphQLEndpoint(), "github",
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+c.Token)
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		},
		graphQLRequest{Query: query, Variables: vars}, &envelope)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("POST "+op, code)
	}
	if len(envelope.Errors) > 0 {
		return &GraphQLErrors{Op: op, Errors: envelope.Errors}
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("github: %s: graphql response carried no data", op)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("github: %s: decode graphql data: %w", op, err)
	}
	return nil
}
