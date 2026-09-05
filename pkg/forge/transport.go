package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

// DoJSON performs one JSON-over-HTTP API call shared by every outbound
// AdminClient (github/gitlab/forgejo). body (when non-nil) is
// JSON-encoded and Content-Type set; out (when non-nil and the status is
// 2xx) is JSON-decoded. The per-driver setHeaders callback applies the
// auth scheme + any provider-specific headers (the token is never placed
// in the URL, so it cannot leak through error strings). errPrefix names
// the provider in wrapped errors ("gitlab"/"github"/"forgejo").
//
// The drivers differ only in (url, setHeaders, errPrefix); this body —
// marshal, request, decode, drain-on-error — was previously copy-pasted
// three times.
func DoJSON(ctx context.Context, client *http.Client, method, url, errPrefix string, setHeaders func(*http.Request), body, out any) (int, error) {
	code, _, err := DoJSONErrBody(ctx, client, method, url, errPrefix, setHeaders, body, out)
	return code, err
}

// DoJSONErrBody is DoJSON that hands back the response body of a NON-2xx
// answer (capped at 8 KiB) instead of draining it, for the calls whose
// refusal reason the operator needs verbatim (an avatar the forge rejects).
// A 2xx body is still streamed into out and never returned.
func DoJSONErrBody(ctx context.Context, client *http.Client, method, url, errPrefix string, setHeaders func(*http.Request), body, out any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: marshal body: %w", errPrefix, err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, nil, err
	}
	if setHeaders != nil {
		setHeaders(req)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return resp.StatusCode, errBody, nil
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("%s: decode response: %w", errPrefix, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	}
	return resp.StatusCode, nil, nil
}

// StatusErr maps a non-2xx status to the appropriate forge sentinel,
// falling back to a "<prefix>: <op>: HTTP <code>" error. Shared by every
// AdminClient so the 401/403/404 mapping stays identical across providers.
func StatusErr(errPrefix, op string, code int) error {
	switch code {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrHookNotFound
	default:
		return fmt.Errorf("%s: %s: HTTP %d", errPrefix, op, code)
	}
}

// DoMultipartFile performs one multipart/form-data upload carrying a single
// file part — the shape GitLab's avatar endpoint takes — with DoJSON's header
// strategy (the token never rides the URL). Unlike DoJSON it hands the response
// body back on every status (capped at 8 KiB): an upload refusal names its
// reason there ("is too big", "content type is invalid"), and the operator
// needs that verbatim. out (when non-nil, on a 2xx with a body) is JSON-decoded.
func DoMultipartFile(ctx context.Context, client *http.Client, method, url, errPrefix string, setHeaders func(*http.Request), field, filename, contentType string, data []byte, out any) (int, []byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscape(field), quoteEscape(filename)))
	// Named explicitly: multipart.CreateFormFile would stamp
	// application/octet-stream, and an upload validator may read the part's
	// own type before sniffing the bytes.
	hdr.Set("Content-Type", stripCRLF(contentType))
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: multipart: %w", errPrefix, err)
	}
	if _, err := part.Write(data); err != nil {
		return 0, nil, fmt.Errorf("%s: multipart: %w", errPrefix, err)
	}
	if err := mw.Close(); err != nil {
		return 0, nil, fmt.Errorf("%s: multipart: %w", errPrefix, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, &buf)
	if err != nil {
		return 0, nil, err
	}
	if setHeaders != nil {
		setHeaders(req)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if out != nil && resp.StatusCode/100 == 2 && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, body, fmt.Errorf("%s: decode response: %w", errPrefix, err)
		}
	}
	return resp.StatusCode, body, nil
}

// quoteEscaper makes a value safe inside a quoted multipart header
// parameter. CR and LF are REMOVED, not escaped: a newline in a header value
// is a new header, and a caller-supplied name would otherwise inject one
// (a second Content-Type ahead of the part's own).
var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "")

func quoteEscape(s string) string { return quoteEscaper.Replace(s) }

var crlfStripper = strings.NewReplacer("\r", "", "\n", "")

func stripCRLF(s string) string { return crlfStripper.Replace(s) }

// TrimBody flattens a response body into one short line, for an error message
// that quotes what the forge said without pasting a page of HTML into a log.
// The cut lands on a rune boundary: the line ends up on a connection record
// and in the studio, where a torn multi-byte character reads as mojibake.
func TrimBody(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
