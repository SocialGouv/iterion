package exec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// buildRequest turns declared parameters into an HTTP request, refusing
// locally — before anything is sent — everything it can decide on its own.
//
// The refusals are the point. A call that reaches the vendor with a
// misspelled argument silently dropped is worse than one that never leaves:
// the vendor answers 200, the workflow checkpoints success, and the thing the
// author asked for did not happen.
func (e *Executor) buildRequest(ctx context.Context, pkg *spec.Package, op spec.Operation, params map[string]any, cred Credential) (*http.Request, error) {
	if err := e.checkAuthorized(pkg, op, cred); err != nil {
		return nil, err
	}
	byKey, err := checkParams(op, params)
	if err != nil {
		return nil, err
	}

	target, err := e.resolveURL(pkg, op, byKey)
	if err != nil {
		return nil, err
	}
	query, err := buildQuery(op, byKey)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	body, contentType, err := buildBody(op, byKey)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, op.HTTP.Method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("exec: operation %q: build request: %w", op.ID, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	accept := op.HTTP.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if e.UserAgent != "" {
		req.Header.Set("User-Agent", e.UserAgent)
	}
	if err := applyHeaderParams(req, op, byKey); err != nil {
		return nil, err
	}
	if err := applyCredential(req, pkg, op, cred); err != nil {
		return nil, err
	}
	return req, nil
}

// checkAuthorized verifies the credential can perform the operation BEFORE
// the call. A binding accepted at launch and refused by the vendor mid-run is
// the failure this prevents, and its message names what is missing.
func (e *Executor) checkAuthorized(pkg *spec.Package, op spec.Operation, cred Credential) error {
	reqs := pkg.EffectiveSecurity(op)
	if len(reqs) == 0 {
		return nil // anonymous, or an API that documents no requirement
	}
	probe := op
	probe.Security = reqs
	probe.Anonymous = false

	ok, missing := probe.SatisfiedBy(cred.SchemeID, cred.Scopes)
	if ok {
		return nil
	}
	// An UNKNOWN grant is not a refusal. A pasted access token carries no
	// scope list, so enforcing scopes against an empty one would make every
	// token-backed connection unusable — the check only bites when the
	// provider actually said what it granted.
	if len(cred.Scopes) == 0 {
		for _, r := range reqs {
			for _, t := range r.Terms {
				if t.SchemeID == cred.SchemeID {
					return nil
				}
			}
		}
	}
	return &Error{
		Class:   spec.ErrForbidden,
		Message: fmt.Sprintf("the connection (scheme %q) cannot perform %s: %s", cred.SchemeID, op.ID, strings.Join(missing, ", ")),
	}
}

// checkParams validates the supplied arguments against the declaration and
// returns them keyed by the operation's public key.
func checkParams(op spec.Operation, params map[string]any) (map[string]spec.Param, error) {
	declared := make(map[string]spec.Param, len(op.Params))
	for _, p := range op.Params {
		declared[p.Key] = p
	}
	// An argument the operation does not declare is an ERROR. Dropping it
	// would let a misspelled key produce a call that succeeds while doing
	// something other than what the author wrote.
	var unknown []string
	for key := range params {
		if _, ok := declared[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, &Error{
			Class:   spec.ErrBadRequest,
			Message: fmt.Sprintf("operation %s does not declare %s (it accepts: %s)", op.ID, quoteList(unknown), strings.Join(sortedKeys(declared), ", ")),
		}
	}

	var missing []string
	out := make(map[string]spec.Param, len(op.Params))
	for _, p := range op.Params {
		v, given := params[p.Key]
		if !given || v == nil {
			if p.Default != nil {
				v = p.Default
			} else if p.Required {
				missing = append(missing, p.Key)
				continue
			} else {
				continue
			}
		}
		if err := checkEnum(op, p, v); err != nil {
			return nil, err
		}
		bound := p
		bound.Default = v // carry the resolved value on the copy
		out[p.Key] = bound
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &Error{
			Class:   spec.ErrBadRequest,
			Message: fmt.Sprintf("operation %s requires %s", op.ID, quoteList(missing)),
		}
	}
	return out, nil
}

// checkEnum refuses a value outside a declared set, locally. The vendor would
// refuse it too, but a run should not spend a network round trip and a rate
// limit slot learning what the package already knows.
func checkEnum(op spec.Operation, p spec.Param, v any) error {
	if len(p.Enum) == 0 {
		return nil
	}
	got := scalarString(v)
	for _, allowed := range p.Enum {
		if got == allowed {
			return nil
		}
	}
	// A secret's VALUE never appears, even when it is the thing being
	// refused. This message reaches the node's event hooks and the run's
	// events.jsonl, which are read by people and shipped to error tracking;
	// `Secret: true` is the package saying "not there".
	shown := strconv.Quote(got)
	if p.Secret {
		shown = "(the value is secret)"
	}
	return &Error{
		Class:   spec.ErrBadRequest,
		Message: fmt.Sprintf("operation %s: %s = %s is not one of %s", op.ID, p.Key, shown, strings.Join(p.Enum, ", ")),
	}
}

// resolveURL builds the target from the connection's origin, the package's
// path prefix and the operation's templated path.
func (e *Executor) resolveURL(pkg *spec.Package, op spec.Operation, byKey map[string]spec.Param) (*url.URL, error) {
	base := e.BaseURL
	if base == "" {
		base = pkg.Connector.BaseURL.Default
	}
	if base == "" {
		return nil, &Error{
			Class:   spec.ErrBadRequest,
			Message: fmt.Sprintf("connector %q is operator-supplied and the connection names no instance URL", pkg.Connector.ID),
		}
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, &Error{Class: spec.ErrBadRequest, Message: fmt.Sprintf("instance URL %q does not parse: %v", base, err)}
	}

	// Both forms are built: the DECODED path Go reasons about, and the
	// ESCAPED one that actually goes on the wire. Setting only the decoded
	// one lets net/url re-escape a value that is already escaped (`a/b`
	// became `a%252Fb`); setting only the escaped one leaves URL.Path
	// disagreeing with it. A value containing `/` must stay ONE segment, or
	// an issue named "a/b" addresses a different resource than the one the
	// workflow named.
	decoded, escaped := op.HTTP.Path, op.HTTP.Path
	for _, p := range byKey {
		if p.In != spec.InPath {
			continue
		}
		raw := scalarString(p.Default)
		decoded = strings.ReplaceAll(decoded, "{"+p.Name+"}", raw)
		escaped = strings.ReplaceAll(escaped, "{"+p.Name+"}", url.PathEscape(raw))
	}
	if strings.ContainsRune(decoded, '{') {
		// Validation makes this unreachable for a loaded package; keeping it
		// means a hand-built operation cannot send a templated URL either.
		return nil, &Error{Class: spec.ErrBadRequest, Message: fmt.Sprintf("operation %s: path %q still has an unfilled placeholder", op.ID, decoded)}
	}
	prefix := strings.TrimRight(u.Path, "/") + pkg.Connector.BaseURL.PathPrefix
	u.Path = prefix + decoded
	u.RawPath = prefix + escaped
	return u, nil
}

// buildQuery places the query parameters, honouring each one's serialization.
func buildQuery(op spec.Operation, byKey map[string]spec.Param) (url.Values, error) {
	out := url.Values{}
	for _, key := range sortedKeys(byKey) {
		p := byKey[key]
		if p.In != spec.InQuery {
			continue
		}
		values, err := serializeValue(op, p, p.Default)
		if err != nil {
			return nil, err
		}
		for _, v := range values {
			out.Add(p.Name, v)
		}
	}
	return out, nil
}

// serializeValue renders one value per its style — the difference between
// `a,b` and two separate `k=v` pairs, which is not cosmetic: only one of them
// is what the vendor parses.
func serializeValue(op spec.Operation, p spec.Param, v any) ([]string, error) {
	items, isList := listOf(v)
	if !isList {
		return []string{scalarString(v)}, nil
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = scalarString(it)
	}
	style := p.Style
	if style == "" {
		style = spec.StyleForm
	}
	switch style {
	case spec.StyleForm:
		if p.ExplodeOrDefault() {
			return parts, nil // repeated k=v pairs
		}
		return []string{strings.Join(parts, ",")}, nil
	case spec.StyleSimple:
		return []string{strings.Join(parts, ",")}, nil
	case spec.StyleSpaceDelimited:
		return []string{strings.Join(parts, " ")}, nil
	case spec.StylePipeDelimited:
		return []string{strings.Join(parts, "|")}, nil
	}
	// A style this build cannot serialize must not fall back to a guess: the
	// guess would be a wrong request that looks right.
	return nil, &Error{
		Class:   spec.ErrBadRequest,
		Message: fmt.Sprintf("operation %s: parameter %s declares serialization style %q, which this build cannot produce", op.ID, p.Key, style),
	}
}

// applyHeaderParams places the header parameters.
func applyHeaderParams(req *http.Request, op spec.Operation, byKey map[string]spec.Param) error {
	for _, key := range sortedKeys(byKey) {
		p := byKey[key]
		if p.In != spec.InHeader {
			continue
		}
		values, err := serializeValue(op, p, p.Default)
		if err != nil {
			return err
		}
		req.Header.Set(p.Name, strings.Join(values, ","))
	}
	return nil
}

// buildBody assembles the request body in the operation's declared encoding.
func buildBody(op spec.Operation, byKey map[string]spec.Param) (io.Reader, string, error) {
	members := map[string]spec.Param{}
	for key, p := range byKey {
		if p.In == spec.InBody {
			members[key] = p
		}
	}
	if len(members) == 0 {
		return nil, "", nil
	}
	switch op.HTTP.RequestBody {
	case spec.BodyJSON:
		payload := map[string]any{}
		for _, p := range members {
			assignBodyMember(payload, p)
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, "", fmt.Errorf("exec: operation %q: encode JSON body: %w", op.ID, err)
		}
		return newBody(raw), "application/json", nil

	case spec.BodyForm:
		form := url.Values{}
		for _, key := range sortedKeys(members) {
			p := members[key]
			values, err := serializeValue(op, p, p.Default)
			if err != nil {
				return nil, "", err
			}
			for _, v := range values {
				form.Add(p.Name, v)
			}
		}
		return newBody([]byte(form.Encode())), "application/x-www-form-urlencoded", nil

	case spec.BodyMultipart:
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, key := range sortedKeys(members) {
			p := members[key]
			values, err := serializeValue(op, p, p.Default)
			if err != nil {
				return nil, "", err
			}
			for _, v := range values {
				if err := w.WriteField(p.Name, v); err != nil {
					return nil, "", fmt.Errorf("exec: operation %q: write multipart field %q: %w", op.ID, p.Name, err)
				}
			}
		}
		if err := w.Close(); err != nil {
			return nil, "", fmt.Errorf("exec: operation %q: close multipart body: %w", op.ID, err)
		}
		return newBody(buf.Bytes()), w.FormDataContentType(), nil
	}
	return nil, "", fmt.Errorf("exec: operation %q: request body encoding %q is not one this build can produce", op.ID, op.HTTP.RequestBody)
}

// assignBodyMember places a member, honouring a nested BodyPath so a flat
// `params:` map can still address a vendor's nested envelope.
func assignBodyMember(payload map[string]any, p spec.Param) {
	path := p.BodyPath
	if path == "" {
		payload[p.Name] = p.Default
		return
	}
	segs := strings.Split(path, ".")
	cur := payload
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = p.Default
}

// applyCredential places the credential where the operation's scheme says.
func applyCredential(req *http.Request, pkg *spec.Package, op spec.Operation, cred Credential) error {
	if cred.SchemeID == "" {
		return nil // anonymous
	}
	scheme, ok := pkg.Connector.AuthScheme(cred.SchemeID)
	if !ok {
		return &Error{
			Class:   spec.ErrUnauthorized,
			Message: fmt.Sprintf("the connection names auth scheme %q, which connector %q does not declare", cred.SchemeID, pkg.Connector.ID),
		}
	}
	switch scheme.Kind {
	case spec.AuthBasic:
		if cred.Username == "" {
			return &Error{Class: spec.ErrUnauthorized, Message: "basic auth needs a username"}
		}
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password)))
		return nil
	case spec.AuthBearer:
		if cred.Value == "" {
			return &Error{Class: spec.ErrUnauthorized, Message: "the connection holds no token"}
		}
		req.Header.Set("Authorization", "Bearer "+cred.Value)
		return nil
	case spec.AuthOAuth2:
		if cred.Value == "" {
			return &Error{Class: spec.ErrUnauthorized, Message: "the connection holds no access token"}
		}
		req.Header.Set("Authorization", "Bearer "+cred.Value)
		return nil
	case spec.AuthAPIKey:
		if cred.Value == "" {
			return &Error{Class: spec.ErrUnauthorized, Message: "the connection holds no key"}
		}
		// The prefix comes from the PACKAGE, never from the stored value, so
		// a re-pasted credential cannot end up double-prefixed — and a
		// missing one cannot silently produce a 401 that reads like a bad
		// token (Forgejo's "token " is exactly this case).
		value := scheme.ValuePrefix + cred.Value
		switch scheme.In {
		case "header":
			req.Header.Set(scheme.Name, value)
		case "query":
			q := req.URL.Query()
			q.Set(scheme.Name, value)
			req.URL.RawQuery = q.Encode()
		default:
			return fmt.Errorf("exec: auth scheme %q has no usable location", scheme.ID)
		}
		return nil
	}
	return fmt.Errorf("exec: auth scheme %q has kind %q, which this build cannot apply", scheme.ID, scheme.Kind)
}

// --- small helpers ---------------------------------------------------------

// listOf reports whether v is a list, and its elements. A string is never a
// list, which matters because []byte and string both range.
func listOf(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// scalarString renders a scalar for a URL or a form field. It is deliberately
// narrow: a float that happens to be integral renders without a decimal
// point, because `page=1` and `page=1.0` are not the same request to a vendor
// that parses integers strictly — and JSON decoding makes every number a
// float64 on the way in.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func quoteList(in []string) string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, ", ")
}
