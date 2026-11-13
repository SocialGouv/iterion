// Ported from rust/crates/runtime/src/bash_validation.rs — pipeline and security gate extensions.
package tools

import (
	"fmt"
	"strings"
)

// PipelineSegment represents a single command in a pipeline/chain.
type PipelineSegment struct {
	Command  string
	Operator string // "|", "&&", "||", ";", or "" for the first segment
}

// SplitPipeline splits a command string on |, &&, ||, ; while respecting
// single quotes, double quotes, and escape sequences.
func SplitPipeline(command string) []PipelineSegment {
	var segments []PipelineSegment
	var current strings.Builder
	currentOp := ""

	inSingle := false
	inDouble := false
	escaped := false

	runes := []rune(command)
	n := len(runes)

	for i := 0; i < n; i++ {
		ch := runes[i]

		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}

		if ch == '\\' && !inSingle {
			current.WriteRune(ch)
			escaped = true
			continue
		}

		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteRune(ch)
			continue
		}

		if ch == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteRune(ch)
			continue
		}

		if inSingle || inDouble {
			current.WriteRune(ch)
			continue
		}

		// Check for operators outside quotes
		if ch == '|' && i+1 < n && runes[i+1] == '|' {
			// ||
			seg := strings.TrimSpace(current.String())
			if seg != "" || len(segments) > 0 {
				segments = append(segments, PipelineSegment{Command: seg, Operator: currentOp})
			}
			currentOp = "||"
			current.Reset()
			i++ // skip second |
			continue
		}

		if ch == '&' && i+1 < n && runes[i+1] == '&' {
			seg := strings.TrimSpace(current.String())
			if seg != "" || len(segments) > 0 {
				segments = append(segments, PipelineSegment{Command: seg, Operator: currentOp})
			}
			currentOp = "&&"
			current.Reset()
			i++ // skip second &
			continue
		}

		if ch == '|' {
			seg := strings.TrimSpace(current.String())
			if seg != "" || len(segments) > 0 {
				segments = append(segments, PipelineSegment{Command: seg, Operator: currentOp})
			}
			currentOp = "|"
			current.Reset()
			continue
		}

		if ch == ';' {
			seg := strings.TrimSpace(current.String())
			if seg != "" || len(segments) > 0 {
				segments = append(segments, PipelineSegment{Command: seg, Operator: currentOp})
			}
			currentOp = ";"
			current.Reset()
			continue
		}

		current.WriteRune(ch)
	}

	// Final segment
	seg := strings.TrimSpace(current.String())
	if seg != "" || len(segments) > 0 {
		segments = append(segments, PipelineSegment{Command: seg, Operator: currentOp})
	}

	return segments
}

// DetectCommandSubstitution returns true if the command contains $(...) or
// backtick substitution outside single quotes.
func DetectCommandSubstitution(command string) bool {
	inSingle := false
	inDouble := false
	escaped := false

	runes := []rune(command)
	n := len(runes)

	for i := 0; i < n; i++ {
		ch := runes[i]

		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}

		// Inside single quotes everything is literal
		if inSingle {
			continue
		}

		// $(...) is active in unquoted context and inside double quotes
		if ch == '$' && i+1 < n && runes[i+1] == '(' {
			return true
		}

		// Backticks
		if ch == '`' {
			return true
		}
	}

	return false
}

// DetectSudoElevatedFlags checks for sudo -E (preserve env) and sudo -u (run as user).
// Returns nil if no elevated flags detected.
func DetectSudoElevatedFlags(command string) *ValidationResult {
	parts := strings.Fields(command)
	if len(parts) == 0 || parts[0] != "sudo" {
		return nil
	}

	for i := 1; i < len(parts); i++ {
		p := parts[i]
		if p == "-E" || p == "--preserve-env" || strings.HasPrefix(p, "--preserve-env=") {
			return &ValidationResult{
				Kind:    ValidationWarn,
				Message: "sudo -E preserves environment, potentially exposing sensitive vars",
			}
		}
		if p == "-u" {
			return &ValidationResult{
				Kind:    ValidationWarn,
				Message: "sudo -u runs as another user",
			}
		}
		// Stop at first non-flag argument (the actual command)
		if !strings.HasPrefix(p, "-") {
			break
		}
	}

	return nil
}

// ValidateArchiveExtraction checks tar/unzip for path safety.
func ValidateArchiveExtraction(command string) ValidationResult {
	first := extractFirstCommand(command)

	if first == "tar" {
		fields := strings.Fields(command)
		hasExtract := false
		hasTargetDir := false
		for _, f := range fields {
			if strings.Contains(f, "x") && (f == "x" || strings.HasPrefix(f, "-") || len(f) <= 5) {
				// Check flags like "xf", "xzf", "-xf", "-xzf"
				stripped := strings.TrimLeft(f, "-")
				if strings.ContainsRune(stripped, 'x') {
					hasExtract = true
				}
			}
			if f == "-C" || strings.HasPrefix(f, "--directory") {
				hasTargetDir = true
			}
			if strings.Contains(f, "..") {
				return ValidationResult{
					Kind:    ValidationWarn,
					Message: "Archive extraction contains path traversal '..' — potential directory escape",
				}
			}
		}
		if hasExtract && !hasTargetDir {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "tar extract without -C (target directory) — files will extract to current directory",
			}
		}
	}

	if first == "unzip" {
		fields := strings.Fields(command)
		hasTargetDir := false
		for _, f := range fields {
			if f == "-d" {
				hasTargetDir = true
			}
			if strings.Contains(f, "..") {
				return ValidationResult{
					Kind:    ValidationWarn,
					Message: "Archive extraction contains path traversal '..' — potential directory escape",
				}
			}
		}
		if !hasTargetDir {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "unzip without -d (target directory) — files will extract to current directory",
			}
		}
	}

	return ValidationResult{Kind: ValidationAllow}
}

// sensitiveEnvPatterns are substrings that indicate sensitive environment variables.
var sensitiveEnvPatterns = []string{
	"API_KEY", "SECRET", "PASSWORD", "TOKEN", "CREDENTIAL", "PRIVATE_KEY",
}

// DetectEnvVarLeak checks for commands that might leak sensitive environment variables.
func DetectEnvVarLeak(command string) ValidationResult {
	upper := strings.ToUpper(command)

	// Check echo/printf $SENSITIVE_VAR patterns
	for _, pat := range sensitiveEnvPatterns {
		// echo $API_KEY, echo ${API_KEY}, printf $SECRET, etc.
		if (strings.Contains(upper, "ECHO ") || strings.Contains(upper, "PRINTF ")) &&
			strings.Contains(upper, pat) &&
			(strings.Contains(command, "$") || strings.Contains(upper, "ENV")) {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: fmt.Sprintf("Command may leak sensitive environment variable containing '%s'", pat),
			}
		}

		// printenv API_KEY
		if strings.Contains(upper, "PRINTENV") && strings.Contains(upper, pat) {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: fmt.Sprintf("Command may leak sensitive environment variable containing '%s'", pat),
			}
		}

		// env | grep SECRET
		if strings.Contains(upper, "ENV") && strings.Contains(upper, "GREP") && strings.Contains(upper, pat) {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: fmt.Sprintf("Command may leak sensitive environment variable containing '%s'", pat),
			}
		}
	}

	return ValidationResult{Kind: ValidationAllow}
}

// ValidateNetworkTimeout warns when network commands lack explicit timeout flags.
func ValidateNetworkTimeout(command string) ValidationResult {
	first := extractFirstCommand(command)

	switch first {
	case "curl":
		if !strings.Contains(command, "--connect-timeout") &&
			!strings.Contains(command, "--max-time") &&
			!strings.Contains(command, " -m ") &&
			!strings.HasSuffix(command, " -m") {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "curl without explicit timeout — consider adding --connect-timeout or --max-time",
			}
		}
	case "wget":
		if !strings.Contains(command, "--timeout") &&
			!strings.Contains(command, "-T") {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "wget without explicit timeout — consider adding --timeout",
			}
		}
	case "ssh":
		if !strings.Contains(command, "ConnectTimeout") {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "ssh without ConnectTimeout — consider adding -o ConnectTimeout=N",
			}
		}
	}

	return ValidationResult{Kind: ValidationAllow}
}
