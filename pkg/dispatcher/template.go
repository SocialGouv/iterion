package dispatcher

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// Template is a parsed dispatch.vars / dispatch.attachments template.
// It supports {{issue.path}} and {{dispatcher.path}} references with the
// same brace syntax used by .bot workflows (but with a narrower set of
// resolvable namespaces).
type Template struct {
	raw      string
	segments []templateSegment
}

type templateSegment struct {
	literal   string   // non-empty when this is a plain-text run
	namespace string   // "issue" | "dispatcher" when this is a reference
	path      []string // dot path under the namespace
	raw       string   // original "{{...}}" form for error messages
}

// ParseTemplate parses s into a Template, validating brace pairing,
// known namespaces, and supported paths. The set of valid paths is
// closed at parse time so missing-key errors surface during config
// validation rather than at dispatch.
func ParseTemplate(s string) (*Template, error) {
	t := &Template{raw: s}
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start == -1 {
			if rest != "" {
				t.segments = append(t.segments, templateSegment{literal: rest})
			}
			break
		}
		if start > 0 {
			t.segments = append(t.segments, templateSegment{literal: rest[:start]})
		}
		end := strings.Index(rest[start:], "}}")
		if end == -1 {
			return nil, fmt.Errorf("unterminated template expression: %q", rest[start:])
		}
		end += start
		raw := rest[start : end+2]
		expr := strings.TrimSpace(rest[start+2 : end])
		seg, err := parseTemplateRef(expr, raw)
		if err != nil {
			return nil, err
		}
		t.segments = append(t.segments, seg)
		rest = rest[end+2:]
	}
	return t, nil
}

func parseTemplateRef(expr, raw string) (templateSegment, error) {
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return templateSegment{}, fmt.Errorf("invalid reference %s: expected namespace.path", raw)
	}
	ns := parts[0]
	switch ns {
	case "issue":
		if !isKnownIssuePath(parts[1:]) {
			return templateSegment{}, fmt.Errorf("invalid reference %s: unknown issue field %q", raw, strings.Join(parts[1:], "."))
		}
	case "dispatcher":
		if !isKnownDispatcherPath(parts[1:]) {
			return templateSegment{}, fmt.Errorf("invalid reference %s: unknown dispatcher field %q", raw, strings.Join(parts[1:], "."))
		}
	default:
		return templateSegment{}, fmt.Errorf("invalid reference %s: unknown namespace %q (issue | dispatcher)", raw, ns)
	}
	return templateSegment{namespace: ns, path: parts[1:], raw: raw}, nil
}

func isKnownIssuePath(p []string) bool {
	if len(p) == 0 {
		return false
	}
	switch p[0] {
	case "id", "identifier", "title", "body", "state", "workflow_state",
		"priority", "assignee", "labels", "labels_list", "url",
		"created_at", "updated_at":
		return len(p) == 1
	case "fields":
		return len(p) == 2 // e.g. issue.fields.severity
	case "metadata":
		return len(p) == 2 // e.g. issue.metadata.html_url
	}
	return false
}

func isKnownDispatcherPath(p []string) bool {
	if len(p) != 1 {
		return false
	}
	switch p[0] {
	case "name", "run_id", "workspace_path", "attempt":
		return true
	}
	return false
}

// TemplateContext is the variable bundle used to render a Template.
type TemplateContext struct {
	Issue      tracker.Issue
	Dispatcher DispatcherVars
}

// DispatcherVars exposes per-dispatch context to templates.
type DispatcherVars struct {
	Name          string
	RunID         string
	WorkspacePath string
	Attempt       int
}

// Render produces the final string. Missing nested values (e.g. an
// issue.fields key that the tracker didn't supply) render as empty.
func (t *Template) Render(ctx TemplateContext) (string, error) {
	if t == nil {
		return "", nil
	}
	var sb strings.Builder
	for _, seg := range t.segments {
		if seg.literal != "" {
			sb.WriteString(seg.literal)
			continue
		}
		value, err := resolveRef(seg.namespace, seg.path, ctx)
		if err != nil {
			return "", fmt.Errorf("render %s: %w", seg.raw, err)
		}
		sb.WriteString(value)
	}
	return sb.String(), nil
}

func resolveRef(ns string, path []string, ctx TemplateContext) (string, error) {
	switch ns {
	case "issue":
		return resolveIssue(path, ctx.Issue)
	case "dispatcher":
		return resolveDispatcher(path, ctx.Dispatcher)
	}
	return "", fmt.Errorf("unknown namespace %s", ns)
}

func resolveIssue(path []string, iss tracker.Issue) (string, error) {
	if len(path) == 0 {
		return "", errors.New("empty path")
	}
	switch path[0] {
	case "id":
		return iss.ID, nil
	case "identifier":
		return iss.Identifier, nil
	case "title":
		return iss.Title, nil
	case "body":
		return iss.Body, nil
	case "state", "workflow_state":
		return iss.WorkflowState, nil
	case "priority":
		return strconv.Itoa(iss.Priority), nil
	case "assignee":
		return iss.Assignee, nil
	case "labels":
		return strings.Join(iss.Labels, ","), nil
	case "labels_list":
		return "[" + strings.Join(iss.Labels, ",") + "]", nil
	case "url":
		return iss.Metadata["url"], nil
	case "created_at":
		return iss.CreatedAt.Format("2006-01-02T15:04:05Z"), nil
	case "updated_at":
		return iss.UpdatedAt.Format("2006-01-02T15:04:05Z"), nil
	case "fields":
		if len(path) < 2 {
			return "", errors.New("issue.fields requires a sub-key")
		}
		return formatAny(iss.Fields[path[1]]), nil
	case "metadata":
		if len(path) < 2 {
			return "", errors.New("issue.metadata requires a sub-key")
		}
		return iss.Metadata[path[1]], nil
	}
	return "", fmt.Errorf("unknown issue field %q", path[0])
}

func resolveDispatcher(path []string, c DispatcherVars) (string, error) {
	if len(path) != 1 {
		return "", fmt.Errorf("invalid dispatcher path %v", path)
	}
	switch path[0] {
	case "name":
		return c.Name, nil
	case "run_id":
		return c.RunID, nil
	case "workspace_path":
		return c.WorkspacePath, nil
	case "attempt":
		return strconv.Itoa(c.Attempt), nil
	}
	return "", fmt.Errorf("unknown dispatcher field %q", path[0])
}

func formatAny(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}
