package exec

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// newBody wraps encoded bytes as a request body.
func newBody(raw []byte) io.Reader { return bytes.NewReader(raw) }

// readResponse turns a vendor's answer into a typed result.
//
// The order is the contract. The STATUS is read first, because a 5xx is a
// failure whatever the body says. Then the outcome policy, because an API
// that signals failure inside a 2xx must be believed over its own status —
// Slack answers 200 with `{"ok": false}`, and reading the status alone would
// checkpoint that as a success. Only then does a declared result case decide
// whether the success is complete or merely accepted.
func (e *Executor) readResponse(pkg *spec.Package, op spec.Operation, resp *http.Response, body []byte) Result {
	data, decodeErr := decodeJSON(body)
	res := Result{Status: resp.StatusCode, Data: data}

	if resp.StatusCode >= 400 {
		res.Err = e.httpError(pkg, op, resp, data, body)
		return res
	}
	// A 2xx whose body does not decode is not a success: the workflow's next
	// step would read fields off nothing.
	if decodeErr != nil && len(bytes.TrimSpace(body)) > 0 {
		res.Err = &Error{
			Class:   spec.ErrUpstream,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("the vendor answered %d with a body that is not JSON: %v", resp.StatusCode, decodeErr),
			Cause:   decodeErr,
		}
		return res
	}

	if policy := pkg.EffectiveOutcome(op); policy != nil && policy.SuccessWhen != "" {
		ok, err := evalSuccess(policy.SuccessWhen, data, resp.StatusCode)
		if err != nil {
			// A predicate that cannot be evaluated must not read as success:
			// that would be the status-only behaviour the policy exists to
			// replace, restored silently by a typo.
			res.Err = &Error{
				Class:   spec.ErrUpstream,
				Status:  resp.StatusCode,
				Message: fmt.Sprintf("the outcome predicate %q could not be evaluated: %v", policy.SuccessWhen, err),
				Cause:   err,
			}
			return res
		}
		if !ok {
			res.Err = bodyError(policy, data, resp.StatusCode)
			return res
		}
	}

	for _, c := range op.Results {
		if c.Status == resp.StatusCode {
			res.Pending = c.Pending
			break
		}
	}
	return res
}

// httpError classifies a 4xx/5xx, preferring the vendor's own code when the
// outcome policy says where to find one.
func (e *Executor) httpError(pkg *spec.Package, op spec.Operation, resp *http.Response, data any, body []byte) *Error {
	out := &Error{Status: resp.StatusCode, Class: spec.ClassifyStatus(resp.StatusCode)}

	// A status the PACKAGE documented wins over the range: a vendor that
	// answers 423 for "this repository is archived" means a conflict, which
	// only the declaration can say.
	for _, es := range op.Errors {
		if es.Status == resp.StatusCode && es.Class != "" {
			out.Class = es.Class
			break
		}
	}
	if policy := pkg.EffectiveOutcome(op); policy != nil {
		if code := stringField(data, policy.ErrorCodeField); code != "" {
			out.Code = code
			out.Class = policy.ClassifyCode(code, resp.StatusCode)
		}
		if msg := stringField(data, policy.ErrorMessageField); msg != "" {
			out.Message = msg
		}
	}
	if out.Message == "" {
		out.Message = firstLineOf(body)
	}
	if out.Class == spec.ErrRateLimited {
		out.RetryAfter = e.retryAfter(resp)
	}
	return out
}

// bodyError builds the failure an outcome predicate rejected — a 2xx the
// vendor nonetheless means as a failure.
func bodyError(policy *spec.OutcomePolicy, data any, status int) *Error {
	code := stringField(data, policy.ErrorCodeField)
	out := &Error{
		Status: status,
		Code:   code,
		Class:  policy.ClassifyCode(code, status),
	}
	out.Message = stringField(data, policy.ErrorMessageField)
	if out.Message == "" {
		if code != "" {
			out.Message = fmt.Sprintf("the vendor answered %d but reported %q", status, code)
		} else {
			out.Message = fmt.Sprintf("the vendor answered %d but its body reports a failure", status)
		}
	}
	return out
}

// retryAfter reads the delay a 429 named. Both forms the RFC allows are
// accepted; an unparsable one yields zero, which the caller reads as "the
// vendor named none" rather than "retry immediately".
func (e *Executor) retryAfter(resp *http.Response) time.Duration {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	d := when.Sub(now())
	if d < 0 {
		return 0
	}
	return d
}

// evalSuccess evaluates an outcome predicate over the decoded body and the
// status.
//
// It runs on pkg/dsl/expr, which is TOTAL — no recursion, bounded
// combinators, a visit budget — so a predicate cannot hang the node it is
// meant to classify. Both `body.<path>` and the bare `status` resolve through
// the Input namespace, which is where expr sends an unknown one.
func evalSuccess(src string, data any, status int) (bool, error) {
	ast, err := expr.Parse(src)
	if err != nil {
		return false, err
	}
	v, err := ast.Eval(&expr.Context{
		Input: func(path []string) any {
			if len(path) == 0 {
				return nil
			}
			switch path[0] {
			case "status":
				return int64(status)
			case "body":
				cur := data
				for _, seg := range path[1:] {
					m, ok := cur.(map[string]any)
					if !ok {
						return nil
					}
					cur = m[seg]
				}
				return cur
			}
			return nil
		},
	})
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		// A predicate that does not answer a boolean is a mistake in the
		// package, and treating a non-boolean as truthy is how `body.error`
		// (a non-empty string on FAILURE) would read as success.
		return false, fmt.Errorf("the predicate answered %T, not a boolean", v)
	}
	return b, nil
}

// firstLineOf gives an error message something to carry when the vendor's
// body is prose rather than a coded object, bounded so a stack trace does not
// become the run's error text.
func firstLineOf(body []byte) string {
	s := strings.TrimSpace(string(body))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxMessage = 300
	if len(s) > maxMessage {
		s = s[:maxMessage] + "…"
	}
	return s
}
