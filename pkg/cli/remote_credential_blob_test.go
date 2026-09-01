package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A credentials.json copied out of a terminal carries ANSI escapes. The
// server used to reject it with a parse error pointing at "\x1b" —
// accurate and useless. It must be normalised here, and the result must
// still be the real payload (values untouched).
func TestReadCredentialBlob_StripsTerminalEscapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Colour runs, a cursor move and an OSC title — the shapes a `cat`
	// under a pager produces — wrapped around a real blob.
	captured := "\x1b[?25l\x1b]0;credentials.json\x07\x1b[32m{\"claudeAiOauth\":{\"accessToken\":\"sk-ant-oat01-SECRET\"," +
		"\"refreshToken\":\"sk-ant-ort01-REFRESH\",\"expiresAt\":1803818276000}}\x1b[0m\x1b[?25h\n"
	if err := os.WriteFile(path, []byte(captured), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCredentialBlob("", path, "claude_code")
	if err != nil {
		t.Fatalf("ReadCredentialBlob: %v", err)
	}
	want := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-SECRET","refreshToken":"sk-ant-ort01-REFRESH","expiresAt":1803818276000}}`
	if string(got) != want {
		t.Fatalf("normalised blob =\n%s\nwant\n%s", got, want)
	}
}

// A clean file is passed through untouched — normalising must not be a
// rewrite.
func TestReadCredentialBlob_CleanPayloadUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	clean := `{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","account_id":"acc"}}`
	if err := os.WriteFile(path, []byte(clean+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCredentialBlob("", path, "codex")
	if err != nil {
		t.Fatalf("ReadCredentialBlob: %v", err)
	}
	if string(got) != clean {
		t.Fatalf("clean payload was rewritten:\n%s", got)
	}
}

// Stripping is a normalisation, not a rescue: what is still not the
// expected shape after it is refused HERE, naming the file, the JSON
// error and the shape — instead of travelling to a server-side 400.
func TestReadCredentialBlob_RefusesWithAnActionableError(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, content, kind string
		wantIn              []string
	}{
		{
			name:    "not json at all",
			content: "\x1b[32mLogin successful. Token: sk-ant-oat01-X\x1b[0m",
			kind:    "claude_code",
			wantIn:  []string{"not a JSON object", "claudeAiOauth"},
		},
		{
			name:    "json object of the wrong shape",
			content: `{"access_token":"x","expires":1}`,
			kind:    "claude_code",
			wantIn:  []string{`no "claudeAiOauth" key`, "access_token", "expires"},
		},
		{
			name:    "codex blob posted as claude_code",
			content: `{"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`,
			kind:    "claude_code",
			wantIn:  []string{`no "claudeAiOauth" key`, "credentials.json"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadCredentialBlob("", path, c.kind)
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error must name the source file, got: %s", msg)
			}
			for _, w := range c.wantIn {
				if !strings.Contains(msg, w) {
					t.Errorf("error must mention %q, got: %s", w, msg)
				}
			}
		})
	}
}

// An unknown kind keeps working: the CLI pins the shapes it knows and
// stays out of the way for anything else (the server remains the
// authority on validity).
func TestReadCredentialBlob_UnknownKindOnlyRequiresJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "other.json")
	if err := os.WriteFile(path, []byte(`{"whatever":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialBlob("", path, "some_future_kind"); err != nil {
		t.Fatalf("unknown kind must not be pinned to a shape: %v", err)
	}
}

// The bytes this returns are the bytes the server SEALS and FINGERPRINTS.
// For a Claude Code credentials.json that fingerprint is the subscription's
// usage-cap identity — secrets.SubscriptionFingerprint has no account id to
// key on and hashes the whole blob — so any incidental rewrite here opens a
// second meter for one subscription. Stripping escapes is the one rewrite
// this function is allowed; surrounding whitespace must survive byte for
// byte, on BOTH input paths (they normalise differently upstream, and this
// must not paper over that either).
func TestReadCredentialBlob_PreservesTheBytesTheServerFingerprints(t *testing.T) {
	blob := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`
	cases := []struct {
		name    string
		payload string
		// want is what the SOURCE hands over, minus escapes only.
		wantFile  string // --from-file: ReadSecretValue already trims trailing \n
		wantStdin string // stdin: ReadSecretBlob hands the raw bytes over
	}{
		{"leading whitespace", "  " + blob, "  " + blob, "  " + blob},
		{"CRLF line ending", blob + "\r\n", blob + "\r", blob + "\r\n"},
		{"trailing spaces", blob + "   ", blob + "   ", blob + "   "},
		{"escapes around a padded blob", "\x1b[32m " + blob + " \x1b[0m", " " + blob + " ", " " + blob + " "},
	}
	for _, c := range cases {
		t.Run(c.name+"/file", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, []byte(c.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadCredentialBlob("", path, "claude_code")
			if err != nil {
				t.Fatalf("ReadCredentialBlob: %v", err)
			}
			if string(got) != c.wantFile {
				t.Errorf("blob = %q, want %q — a rewritten byte re-stamps the subscription", got, c.wantFile)
			}
		})
		t.Run(c.name+"/stdin", func(t *testing.T) {
			got, err := readCredentialBlobFromStdin(t, c.payload)
			if err != nil {
				t.Fatalf("ReadCredentialBlob: %v", err)
			}
			if string(got) != c.wantStdin {
				t.Errorf("blob = %q, want %q — a rewritten byte re-stamps the subscription", got, c.wantStdin)
			}
		})
	}
}

// readCredentialBlobFromStdin exercises the piped path
// (`cat credentials.json | iterion remote admin llm oauth set …`), which
// ReadSecretBlob reads whole from os.Stdin.
func readCredentialBlobFromStdin(t *testing.T, payload string) ([]byte, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "piped")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	saved := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = saved }()
	return ReadCredentialBlob("", "", "claude_code")
}
