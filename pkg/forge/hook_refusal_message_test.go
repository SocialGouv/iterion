package forge

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestRefusalMessage_ReadsEachForgesShape: the operator's remedy depends on
// what the forge said, and the three forges say it in different shapes. Each
// row is a body a forge really answers a refused hook with.
func TestRefusalMessage_ReadsEachForgesShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"gitlab string message", `{"message":"Invalid url given"}`, "Invalid url given"},
		{
			"gitlab field errors, keys in sorted order",
			`{"message":{"url":["is blocked: Requests to the local network are not allowed"],"base":["Maximum number of project hooks (100) exceeded"]}}`,
			"base: Maximum number of project hooks (100) exceeded; url: is blocked: Requests to the local network are not allowed",
		},
		{
			"github validation errors",
			`{"message":"Validation Failed","errors":[{"resource":"Hook","code":"custom","message":"Hook already exists on this repository"}]}`,
			"Validation Failed; Hook already exists on this repository",
		},
		{"error token", `{"error":"insufficient_scope"}`, "insufficient_scope"},
		{"not json: the text itself, on one line", "<html>\n  <body>502 Bad   Gateway</body>\n</html>", "<html> <body>502 Bad Gateway</body> </html>"},
		{"no body", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefusalMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("RefusalMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestRefusalMessage_CapsALongAnswer: a forge (or the proxy in front of it)
// answering a page of text must not push the remedy out of sight.
func TestRefusalMessage_CapsALongAnswer(t *testing.T) {
	got := RefusalMessage([]byte(strings.Repeat("x", 5000)))
	if r := []rune(got); len(r) != hookRefusalMessageMax+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("a 5000-char answer came back as %d runes (suffix %q), want %d + an ellipsis", len(r), got[len(got)-3:], hookRefusalMessageMax)
	}
}

// TestHookWriteErr_CarriesTheMessageAndKeepsTheSentinels: a refusal carries
// the forge's reason on the *StatusError every caller already classifies on,
// and the statuses mapped to sentinels stay sentinels.
func TestHookWriteErr_CarriesTheMessageAndKeepsTheSentinels(t *testing.T) {
	err := HookWriteErr("gitlab", "create hook", http.StatusUnprocessableEntity, []byte(`{"message":"Invalid url given"}`))
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("a 422 must stay a *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusUnprocessableEntity || se.Detail != "Invalid url given" {
		t.Errorf("StatusError = {Code:%d Detail:%q}, want {422 \"Invalid url given\"}", se.Code, se.Detail)
	}
	if !strings.Contains(err.Error(), "(Invalid url given)") {
		t.Errorf("the message must read in the error string too: %v", err)
	}
	if err := HookWriteErr("gitlab", "create hook", http.StatusForbidden, []byte(`{"message":"403 Forbidden"}`)); !errors.Is(err, ErrForbidden) {
		t.Errorf("a 403 must stay the ErrForbidden sentinel the grant remedy is mapped from, got %T: %v", err, err)
	}
}
