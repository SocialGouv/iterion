package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// ModelsOptions configures the `iterion models` command.
type ModelsOptions struct {
	// Spec is an optional "provider/model-id". When empty the command lists a
	// representative set of known models (model.KnownModelSpecs()).
	Spec string
	// Refresh force-refetches the model-spec cache before resolving.
	Refresh bool
}

// modelRow is one resolved model in the output (human + JSON share this shape).
type modelRow struct {
	Spec            string `json:"spec"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Source          string `json:"source"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	Reasoning       bool   `json:"reasoning"`
	ToolCall        bool   `json:"tool_call"`
	Temperature     bool   `json:"temperature"`
}

// modelsResult is the top-level JSON envelope.
type modelsResult struct {
	Refreshed    bool       `json:"refreshed"`
	RefreshError string     `json:"refresh_error,omitempty"`
	Models       []modelRow `json:"models"`
}

// RunModels resolves and prints ModelCapabilities for one model (--spec/arg) or
// a representative known set, reporting the resolution source
// (aggregator|curated) and the context window. With opts.Refresh it first
// force-refetches the model-spec cache via the shared resolver; a failed
// refresh is reported but non-fatal so the command still works offline.
func RunModels(ctx context.Context, opts ModelsOptions, p *Printer) error {
	result := modelsResult{}

	if opts.Refresh {
		result.Refreshed = true
		if err := model.RefreshModelSpecs(ctx); err != nil {
			result.RefreshError = err.Error()
		}
	}

	var specsList []string
	if s := strings.TrimSpace(opts.Spec); s != "" {
		if _, _, err := model.ParseModelSpec(s); err != nil {
			return UserInputError(err)
		}
		specsList = []string{s}
	} else {
		specsList = model.KnownModelSpecs()
	}

	for _, spec := range specsList {
		rc, err := model.ResolveSpec(spec)
		if err != nil {
			return UserInputError(err)
		}
		result.Models = append(result.Models, modelRow{
			Spec:            rc.Spec,
			Provider:        rc.Provider,
			Model:           rc.Model,
			Source:          string(rc.Source),
			ContextWindow:   rc.ContextWindow,
			MaxOutputTokens: rc.MaxOutputTokens,
			Reasoning:       rc.Reasoning,
			ToolCall:        rc.ToolCall,
			Temperature:     rc.Temperature,
		})
	}

	if p.Format == OutputJSON {
		p.JSON(result)
		return nil
	}

	if result.Refreshed {
		if result.RefreshError != "" {
			p.Line("! model-spec refresh failed: %s (showing cached/curated values)", result.RefreshError)
		} else {
			p.Line("✓ model-spec cache refreshed")
		}
		p.Blank()
	}

	p.Header("Model capabilities")
	headers := []string{"MODEL", "SOURCE", "CONTEXT", "MAX OUT", "REASON", "TOOLS", "TEMP"}
	rows := make([][]string, 0, len(result.Models))
	for _, m := range result.Models {
		rows = append(rows, []string{
			m.Spec,
			m.Source,
			formatContextWindow(m.ContextWindow),
			formatContextWindow(m.MaxOutputTokens),
			yesNo(m.Reasoning),
			yesNo(m.ToolCall),
			yesNo(m.Temperature),
		})
	}
	p.Table(headers, rows)
	return nil
}

// formatContextWindow renders a token count compactly (1M, 200K, 4096) and
// "—" when unknown (zero). Shared by the context window and the max-output
// column: both are token counts whose zero means unknown.
func formatContextWindow(n int) string {
	switch {
	case n <= 0:
		return "—"
	case n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n%1_000 == 0:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
