package exec

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// TestScrubSecretsCoversThePathEscapedForm.
//
// A PATH parameter the package marked secret travels PathEscape'd
// (request.go), whose form differs from both the raw bytes and the
// QueryEscape form (`tok&en+ value` → `tok&en+%20value`, with `+` raw where
// QueryEscape sends %2B and sends space as +). A vendor answering 200 with
// the request URI echoed hands that form straight back in Result.Data, and
// scrubSecrets missing it is a leak on the only path that succeeds.
func TestScrubSecretsCoversThePathEscapedForm(t *testing.T) {
	const secret = "tok&en+ value"
	escaped := url.PathEscape(secret)
	if escaped == secret || escaped == url.QueryEscape(secret) {
		t.Fatalf("fixture: %q escapes to %q, indistinguishable from the forms already covered", secret, escaped)
	}
	got := scrubSecrets("repo/"+escaped+"/file", []string{secret})
	if got != "repo/"+redactedMarker+"/file" {
		t.Errorf("scrubbed = %q, want the path-escaped form redacted", got)
	}
}

// TestARedactedTransportErrorCoversThePathEscapedForm: the same third form on
// the error path — redactSecrets rewrites a *url.Error whose URL carries the
// path-escaped parameter, and that text is what reaches the run's events.
func TestARedactedTransportErrorCoversThePathEscapedForm(t *testing.T) {
	const secret = "tok&en+ value"
	op := spec.Operation{
		ID: "probe.file.get", Resource: "file", Verb: "get",
		Params: []spec.Param{{Key: "repo", Name: "repo", In: spec.InPath, Secret: true}},
	}
	err := &url.Error{
		Op:  "Get",
		URL: "https://vendor/api/" + url.PathEscape(secret) + "/file",
		Err: errors.New("connection refused"),
	}
	out := redactSecrets(err, op, map[string]any{"repo": secret}, Credential{})
	if out == nil {
		t.Fatal("redactSecrets lost the error")
	}
	if strings.Contains(out.Error(), url.PathEscape(secret)) {
		t.Errorf("the path-escaped credential survives in the transport error: %s", out.Error())
	}
	if !strings.Contains(out.Error(), redactedMarker) {
		t.Errorf("the redaction must be visible, so a reader knows something was removed: %s", out.Error())
	}
}
