package hooks

import "strings"

// MergeHookFeedback combines hook messages with tool output.
// If messages is empty, output is returned unchanged.
// Otherwise, a "Hook feedback" (or "Hook feedback (error)" when isError)
// section is appended, separated by a blank line.
//
// Matches Rust's merge_hook_feedback() in conversation.rs.
func MergeHookFeedback(messages []string, output string, isError bool) string {
	if len(messages) == 0 {
		return output
	}

	var sections []string
	if strings.TrimSpace(output) != "" {
		sections = append(sections, output)
	}

	label := "Hook feedback"
	if isError {
		label = "Hook feedback (error)"
	}
	sections = append(sections, label+":\n"+strings.Join(messages, "\n"))

	return strings.Join(sections, "\n\n")
}
