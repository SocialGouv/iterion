package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/store"
)

// APIError is a non-2xx response from the remote instance. Body is the raw
// response; Error() says what the server said (describeErrorBody).
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d %s %s: %s", e.Status, e.Method, e.Path, describeErrorBody([]byte(e.Body)))
}

// errorBody is the JSON error body the server answers with: the message
// (error; a refusal that puts a stable token there says it in message or
// detail), a stable code a client acts on (error_code), the remedy a refusal
// names (hint — a resume that would lose its scratch names the consent it
// needs), also_needs_force when the resume needs --force besides, and
// retryable / reset_at when trying again later helps.
type errorBody struct {
	Error          string `json:"error"`
	ErrorCode      string `json:"error_code"`
	Message        string `json:"message"`
	Detail         string `json:"detail"`
	Hint           string `json:"hint"`
	AlsoNeedsForce bool   `json:"also_needs_force"`
	Retryable      bool   `json:"retryable"`
	ResetAt        string `json:"reset_at"`
}

// describeErrorBody is what an error response says to an operator: a JSON
// error body's fields, each in full — the server writes the hint after the
// message, so a cut of the raw body loses it first — else the body's first
// line.
func describeErrorBody(body []byte) string {
	var b errorBody
	if err := json.Unmarshal(body, &b); err != nil || (b.Error == "" && b.Message == "" && b.Detail == "") {
		if msg := firstLine(body); msg != "" {
			return msg
		}
		return "(empty body)"
	}
	var words, facts []string
	for _, w := range []string{b.Error, b.Message, b.Detail} {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, clipField(w))
		}
	}
	if len(words) == 0 {
		// Only blanks where the words should be: the body says more than that.
		if msg := firstLine(body); msg != "" {
			return msg
		}
		return "(empty body)"
	}
	if code := strings.TrimSpace(b.ErrorCode); code != "" {
		facts = append(facts, "error_code: "+clipField(code))
	}
	if b.AlsoNeedsForce {
		facts = append(facts, "also needs --force")
	}
	if b.Retryable {
		facts = append(facts, "retryable")
	}
	if at := strings.TrimSpace(b.ResetAt); at != "" {
		facts = append(facts, "resets at "+clipField(at))
	}
	s := strings.Join(words, " — ")
	if len(facts) > 0 {
		s += " (" + strings.Join(facts, "; ") + ")"
	}
	if hint := strings.TrimSpace(b.Hint); hint != "" {
		s += store.RunErrorHintSeparator + clipField(hint)
	}
	return s
}

// clipField is one decoded field of a server's answer made fit for a
// terminal (terminalText), bounded on a rune boundary: a terminal is not a
// sink for a body of any size.
func clipField(s string) string {
	return terminalText(s, 2000)
}

// terminalText is text a server relayed — a node's error carries the output
// of processes the run executed — made inert for a terminal, then bounded to
// limit runes: every control character (C0, DEL, C1: an escape sequence, a
// bell, a carriage return or newline that forges a line) and every format or
// separator character (bidi overrides and marks, which reorder what is
// shown; line and paragraph separators; tag characters) is written as its
// escape, never sent raw.
func terminalText(s string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			b.WriteString("…")
			break
		}
		n++
		writeInert(&b, r)
	}
	return b.String()
}

// inertText is s made inert for a terminal by terminalText's rule, unbounded,
// its layout kept: line feeds and tabs are written as they are.
func inertText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		writeInert(&b, r)
	}
	return b.String()
}

// writeInert writes r to b as a terminal shows it, escaped when a terminal
// would act on it instead: a byte that is no character, a control character
// (C0, DEL, C1), a format or separator character.
func writeInert(b *strings.Builder, r rune) {
	switch {
	case r == utf8.RuneError:
		b.WriteString(`\ufffd`)
	case unicode.IsControl(r):
		fmt.Fprintf(b, `\x%02x`, r)
	case unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
		fmt.Fprintf(b, `\u%04x`, r)
	default:
		b.WriteRune(r)
	}
}

// Call performs an authenticated JSON request. A non-nil `in` is
// marshalled as the request body; a non-2xx status returns *APIError;
// a non-nil `out` receives the decoded response. The raw response body
// is always returned so --json output can be a lossless passthrough of
// what the server sent.
func (c *RemoteClient) Call(ctx context.Context, method, path string, in, out any) ([]byte, error) {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = b
	}
	code, resp, err := c.API(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return resp, &APIError{Status: code, Method: method, Path: path, Body: string(resp)}
	}
	if out != nil && len(resp) > 0 {
		if err := json.Unmarshal(resp, out); err != nil {
			return resp, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return resp, nil
}

// Upload performs an authenticated multipart/form-data POST with the
// file under `field`, plus optional extra form values. Non-2xx returns
// *APIError; a non-nil `out` receives the decoded response.
func (c *RemoteClient) Upload(ctx context.Context, path, field, filename string, r io.Reader, extra map[string]string, out any) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range extra {
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, r); err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	code, resp, err := c.doWithContentType(ctx, "POST", path, buf.Bytes(), mw.FormDataContentType())
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return resp, &APIError{Status: code, Method: "POST", Path: path, Body: string(resp)}
	}
	if out != nil && len(resp) > 0 {
		if err := json.Unmarshal(resp, out); err != nil {
			return resp, fmt.Errorf("decode POST %s response: %w", path, err)
		}
	}
	return resp, nil
}

// ResolveTeam resolves the team id for a team-scoped command: explicit
// flag > env/persisted default (both already folded into cfg.TeamID by
// ResolveRemoteConfig) > the account's active team from /api/auth/me.
// Never a silent guess: an unresolvable team is an explicit error.
func (c *RemoteClient) ResolveTeam(ctx context.Context, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if c.cfg.TeamID != "" {
		return c.cfg.TeamID, nil
	}
	me, err := c.Me(ctx)
	if err != nil {
		return "", err
	}
	if me.ActiveTeam == "" {
		return "", fmt.Errorf("no team scope: pass --team, set ITERION_REMOTE_TEAM, or run `iterion remote teams switch <id>`")
	}
	return me.ActiveTeam, nil
}

// ResolveOrg is ResolveTeam's org-level counterpart.
func (c *RemoteClient) ResolveOrg(ctx context.Context, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if c.cfg.OrgID != "" {
		return c.cfg.OrgID, nil
	}
	me, err := c.Me(ctx)
	if err != nil {
		return "", err
	}
	if me.ActiveOrg == "" {
		return "", fmt.Errorf("no org scope: pass --org, set ITERION_REMOTE_ORG, or run `iterion remote orgs switch <id>`")
	}
	return me.ActiveOrg, nil
}

// RemoteMe IS the server's /api/auth/me response type, aliased: the
// decode target cannot drift from the wire (a hand-mirrored struct once
// kept a field the server had renamed, silently decoding zero teams).
// Teams nest under Orgs (server.OrgTreeView → server.MembershipView).
type RemoteMe = server.AuthMeResponse

// Me fetches the authenticated account's identity view.
func (c *RemoteClient) Me(ctx context.Context) (RemoteMe, error) {
	var me RemoteMe
	_, err := c.Call(ctx, "GET", "/api/auth/me", nil, &me)
	// The aliased response type carries an AccessToken field the login
	// path uses; /api/auth/me never populates it. Zero it regardless so
	// no future caller can serialize a token out of this struct.
	me.AccessToken = ""
	return me, err
}

// ParseAttachFlags parses repeated "name=path" attachment flags.
func ParseAttachFlags(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	return parseKVPairs[string](flags, kvOpts[string]{
		errFmt:            "invalid --attach format %q (expected name=path)",
		trimKey:           true,
		trimVal:           true,
		requireTrimmedKey: true,
	})
}

// ReadDataArg reads a `--data` argument: literal JSON, or `@file`
// (with `@-` reading stdin). Empty input returns nil.
func ReadDataArg(data string) ([]byte, error) {
	switch {
	case data == "":
		return nil, nil
	case data == "@-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(data, "@"):
		return os.ReadFile(data[1:])
	default:
		return []byte(data), nil
	}
}

// PrintRemoteJSON renders a raw server response: lossless passthrough
// in --json mode, pretty-printed JSON otherwise. The fallback for
// commands whose payload has no table rendering.
func PrintRemoteJSON(p *Printer, body []byte) {
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil && pretty.Len() > 0 {
		p.Line("%s", strings.TrimRight(pretty.String(), "\n"))
		return
	}
	p.Line("%s", strings.TrimRight(string(body), "\n"))
}

// QueryString builds a ?k=v&… query from the non-empty pairs (empty
// string when none are set).
func QueryString(pairs map[string]string) string {
	q := url.Values{}
	for k, v := range pairs {
		if v != "" {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}
