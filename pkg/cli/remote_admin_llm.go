package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
