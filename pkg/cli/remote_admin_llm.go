package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// remote admin llm — platform LLM credentials (super-admin): the DB-backed
// form of the runner-pod env fallback. Values are never taken from argv.
// Key CRUD rides the shared api-keys helpers under scope "platform"
// (remote_secrets.go); only the OAuth connect flow and the blob reader
// are specific to this surface.

func adminLLMOAuthBase(kind string) string { return "/api/admin/llm/oauth/" + kind }

// ReadSecretBlob resolves a possibly multi-line secret payload (e.g. a
// credentials.json) from --from-env, a file, or — unlike ReadSecretValue's
// single-line stdin — the WHOLE of stdin, so a pretty-printed JSON blob
// pipes through intact.
func ReadSecretBlob(fromEnv, fromFile string) ([]byte, error) {
	if fromEnv != "" || fromFile != "" {
		v, err := ReadSecretValue(fromEnv, fromFile, false)
		if err != nil {
			return nil, err
		}
		return []byte(v), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read secret from stdin: %w", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("provide the payload via --from-env <VAR>, --from-file <path>, or pipe it on stdin")
	}
	return b, nil
}

// ansiEscape matches the escape sequences a terminal capture carries:
// CSI (colour, cursor) and OSC (title) runs.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// ReadCredentialBlob resolves a credentials payload like ReadSecretBlob,
// then makes it usable: a blob copied out of a terminal (a `cat` of
// credentials.json under a pager, a screenshot-to-clipboard round trip)
// carries ANSI escapes, and the server rejected it with a parse error
// pointing at "\x1b" — accurate and useless. The escapes are stripped and
// the result is validated HERE, so a malformed payload is named before it
// travels: which file, which JSON error, and what the shape should be.
//
// Stripping is a normalisation, not a rescue: anything that is still not
// the expected object after it is refused, loudly.
func ReadCredentialBlob(fromEnv, fromFile, kind string) ([]byte, error) {
	raw, err := ReadSecretBlob(fromEnv, fromFile)
	if err != nil {
		return nil, err
	}
	clean := bytes.TrimSpace(ansiEscape.ReplaceAll(raw, nil))
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(clean, &probe); err != nil {
		return nil, fmt.Errorf("%s: the payload is not a JSON object (%v)%s", credentialSourceLabel(fromEnv, fromFile), err, credentialShapeHint(kind))
	}
	if want := credentialTopKey(kind); want != "" {
		if _, ok := probe[want]; !ok {
			keys := make([]string, 0, len(probe))
			for k := range probe {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("%s: no %q key — got %v%s", credentialSourceLabel(fromEnv, fromFile), want, keys, credentialShapeHint(kind))
		}
	}
	// The key being PRESENT is not the payload being usable. A key probe
	// reads json.RawMessage, so `{"claudeAiOauth":{}}` — or a value that
	// is not even an object — passes it, and the server refuses it for a
	// missing access token with no file name left to name: exactly the
	// round trip this function exists to close.
	if err := credentialCheckShape(kind, clean); err != nil {
		return nil, fmt.Errorf("%s: %v%s", credentialSourceLabel(fromEnv, fromFile), err, credentialShapeHint(kind))
	}
	return clean, nil
}

// credentialTopKey is the object key each forfait blob must carry; ""
// for a kind whose shape this CLI does not pin. It owns the DIAGNOSTIC
// for the commonest mistake — pasting the other CLI's blob — because it
// can print `no "claudeAiOauth" key — got [auth_mode tokens]`, which a
// typed parse cannot: unmarshalling ignores unknown keys and would
// report an empty access token instead, losing the "wrong file" signal.
// Validity itself is credentialCheckShape's, not this function's.
func credentialTopKey(kind string) string {
	switch kind {
	case string(secrets.OAuthKindClaudeCode):
		return "claudeAiOauth"
	case string(secrets.OAuthKindCodex):
		return "tokens"
	}
	return ""
}

// credentialCheckShape refuses what the server's sealOAuthRecord would
// refuse, by the SAME reading: the shared view parsers are the authority,
// so there is no second copy of the shape here to drift from theirs. The
// access token is the field checked because it is the one that makes the
// blob usable at all — stored without it, no run can spend the forfait.
//
// A kind this CLI does not pin returns nil: the server stays the
// authority on validity, and an unrecognised blob still travels.
func credentialCheckShape(kind string, blob []byte) error {
	switch kind {
	case string(secrets.OAuthKindClaudeCode):
		v, err := secrets.ParseAnthropicView(blob)
		if err != nil {
			return err
		}
		if v.ClaudeAIOauth.AccessToken == "" {
			return errors.New(`"claudeAiOauth" carries no accessToken`)
		}
	case string(secrets.OAuthKindCodex):
		v, err := secrets.ParseCodexView(blob)
		if err != nil {
			return err
		}
		if v.Tokens.AccessToken == "" {
			return errors.New(`"tokens" carries no access_token`)
		}
	}
	return nil
}

func credentialShapeHint(kind string) string {
	switch kind {
	case string(secrets.OAuthKindClaudeCode):
		return "\nexpected the Claude Code credentials.json: {\"claudeAiOauth\":{\"accessToken\":…,\"refreshToken\":…,\"expiresAt\":…}}"
	case string(secrets.OAuthKindCodex):
		return "\nexpected the Codex auth.json: {\"tokens\":{\"access_token\":…,\"refresh_token\":…},\"auth_mode\":\"chatgpt\"}"
	}
	return ""
}

// credentialSourceLabel names WHERE the payload came from, so the error
// points at the thing to fix rather than at an anonymous blob.
//
// The order MIRRORS ReadSecretValue's: env before file. The two flags are
// not mutually exclusive, so with both set the payload is read from the
// env var — and a label that tested the file first would send the
// operator to edit a file that was never opened, in precisely the case
// where they are already confused about which source is live.
func credentialSourceLabel(fromEnv, fromFile string) string {
	switch {
	case fromEnv != "":
		return "$" + fromEnv
	case fromFile != "":
		return fromFile
	}
	return "stdin"
}

// RemoteAdminLLMOAuthConnect drives the browser code flow for the platform
// forfait: authorize/start mints the URL, the operator authorizes in a
// browser and pastes the resulting `code#state`, authorize/complete
// exchanges it server-side into the stored credentials blob.
func RemoteAdminLLMOAuthConnect(ctx context.Context, c *RemoteClient, p *Printer, kind string) error {
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	if _, err := c.Call(ctx, "POST", adminLLMOAuthBase(kind)+"/authorize/start", nil, &start); err != nil {
		return err
	}
	// URL + prompt on stderr so a piped stdout carries ONLY the final JSON
	// result (`oauth connect | jq` must not choke on the human preamble).
	fmt.Fprintln(os.Stderr, "Open this URL in a browser, authorize, then paste the code below:")
	fmt.Fprintln(os.Stderr, "  "+start.AuthorizeURL)
	fmt.Fprint(os.Stderr, "code (code#state): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	code := strings.TrimSpace(line)
	if code == "" {
		if err != nil {
			return fmt.Errorf("read authorization code: %w", err)
		}
		return fmt.Errorf("no authorization code pasted")
	}
	raw, err := c.Call(ctx, "POST", adminLLMOAuthBase(kind)+"/authorize/complete",
		map[string]string{"code": code, "state": start.State}, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}
