package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// The oracle is a read-only senior-engineering advisor: a single call to a
// (usually stronger or higher-effort) model with the question and the
// relevant files inlined — no tools, no session mutation. Use it for
// architecture decisions, gnarly debugging, performance analysis, and plan
// review; announce the consultation to the user.

const (
	// oraclePerFileCap bounds each attached file; beyond it the head is
	// kept with a visible truncation marker (the oracle sees the marker,
	// so partial context is never mistaken for the whole file).
	oraclePerFileCap = 48 * 1024
	// oracleMaxFiles bounds the attachment count.
	oracleMaxFiles = 16
)

// oracleSystemPrompt frames the advisor role. Original text targeting the
// same role as Amp's oracle.
const oracleSystemPrompt = `You are the Oracle: a senior engineering advisor consulted mid-task for architecture decisions, complex debugging, performance analysis, and plan review. You have NO tools — reason strictly from the material provided. Be decisive and concrete: name the recommended option and why it beats the alternatives, list the key risks with mitigations, and end with actionable next steps. If the material is insufficient to answer responsibly, say exactly what is missing instead of hedging.`

// OracleTool returns the tool definition for consulting the oracle.
func OracleTool() api.Tool {
	return api.Tool{
		Name: "oracle",
		Description: "Consult the oracle — a read-only senior-engineering advisor (a stronger or higher-effort " +
			"model) — for architecture decisions, complex debugging, performance analysis, or plan review. " +
			"Provide the question plus the relevant file paths; the files are inlined for it (it has no tools). " +
			"Tell the user you are consulting the oracle, and weigh its advice against what you can verify — it " +
			"only sees what you send.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"question": {Type: "string", Description: "The question or decision to review, with your current thinking."},
				"files": {Type: "array", Description: "Paths of files to attach (max 16; each capped at 48KB with a visible truncation marker).",
					Items: &api.Property{Type: "string"}},
				"context": {Type: "string", Description: "Optional extra context: constraints, error output, what was already tried."},
			},
			Required: []string{"question"},
		},
	}
}

// BuildOraclePrompt validates oracle input and assembles the (system, user)
// prompt pair, inlining the requested files. Relative paths resolve against
// workDir. A missing or unreadable file is an explicit error — silently
// consulting the oracle without the material it was meant to see would
// produce confident advice on the wrong basis.
func BuildOraclePrompt(input map[string]any, workDir string) (system, user string, err error) {
	question := strings.TrimSpace(stringVal(input, "question"))
	if question == "" {
		return "", "", fmt.Errorf("oracle: 'question' is required")
	}
	files := stringSlice(input, "files")
	if len(files) > oracleMaxFiles {
		return "", "", fmt.Errorf("oracle: too many files (%d > %d) — attach only the decisive ones", len(files), oracleMaxFiles)
	}

	var sb strings.Builder
	sb.WriteString(question)
	if extra := strings.TrimSpace(stringVal(input, "context")); extra != "" {
		sb.WriteString("\n\n## Additional context\n")
		sb.WriteString(extra)
	}
	for _, path := range files {
		resolved := path
		if !filepath.IsAbs(resolved) && workDir != "" {
			resolved = filepath.Join(workDir, resolved)
		}
		data, readErr := os.ReadFile(resolved)
		if readErr != nil {
			return "", "", fmt.Errorf("oracle: cannot read %s: %w", path, readErr)
		}
		content := string(data)
		truncated := false
		if len(content) > oraclePerFileCap {
			content = content[:oraclePerFileCap]
			truncated = true
		}
		fmt.Fprintf(&sb, "\n\n## File: %s\n```\n%s\n```", path, content)
		if truncated {
			fmt.Fprintf(&sb, "\n[file truncated at %d bytes — the tail was NOT shown to you]", oraclePerFileCap)
		}
	}
	return oracleSystemPrompt, sb.String(), nil
}
